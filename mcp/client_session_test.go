package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// restartableMCPServer is a Streamable HTTP server that issues a fresh session
// ID on every initialize and answers 404 to any request carrying an ID it has
// not issued since its last restart — the behaviour the spec requires of a
// server whose sessions are gone.
type restartableMCPServer struct {
	*httptest.Server

	mu       sync.Mutex
	issued   map[string]bool
	sessions int

	// toolCallsAlways404 makes tools/call refuse every session, issued or not.
	toolCallsAlways404 bool

	// reissuesIDs makes restart reset the ID counter, so the first session
	// issued afterwards repeats an ID issued before it — the behaviour of a
	// server that numbers sessions in memory. A tools/call carrying no session
	// is refused 400, as the spec asks of a server that requires one.
	reissuesIDs bool

	// issuesNoIDAfterRestart makes every initialize after a restart answer with
	// no session ID: the server now runs without sessions.
	issuesNoIDAfterRestart bool
	restarted              bool

	initializes   atomic.Int64
	initWithSID   atomic.Int64
	toolCalls     atomic.Int64
	toolCalls404  atomic.Int64
	lastCallerSID atomic.Value
}

func newRestartableMCPServer(t *testing.T) *restartableMCPServer {
	t.Helper()
	s := &restartableMCPServer{issued: map[string]bool{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

// restart drops every session, as a process restart does.
func (s *restartableMCPServer) restart() {
	s.mu.Lock()
	s.issued = map[string]bool{}
	if s.reissuesIDs {
		s.sessions = 0
	}
	s.restarted = true
	s.mu.Unlock()
}

func (s *restartableMCPServer) handle(w http.ResponseWriter, r *http.Request) {
	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	sid := r.Header.Get("Mcp-Session-Id")

	if req.Method == mcpMethodInitialize {
		s.initializes.Add(1)
		if sid != "" {
			s.initWithSID.Add(1)
		}
		s.mu.Lock()
		s.sessions++
		newSID := "session-" + strconv.Itoa(s.sessions)
		if s.issuesNoIDAfterRestart && s.restarted {
			newSID = ""
		}
		s.issued[newSID] = true
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if newSID != "" {
			w.Header().Set("Mcp-Session-Id", newSID)
		}
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: initializeResult("restartable", "1")})
		return
	}

	s.mu.Lock()
	known := s.issued[sid]
	s.mu.Unlock()

	if req.Method == "notifications/initialized" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if req.Method == mcpMethodToolsCall {
		s.toolCalls.Add(1)
		s.lastCallerSID.Store(sid)
		if sid == "" && s.reissuesIDs {
			http.Error(w, "missing session", http.StatusBadRequest)
			return
		}
		if !known || s.toolCallsAlways404 {
			s.toolCalls404.Add(1)
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{
			JSONRPC: "2.0", ID: req.ID,
			Result: mustMarshal(ToolCallResult{Content: []ContentBlock{{Type: "text", Text: "ok"}}}),
		})
		return
	}
	if !known {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: mustMarshal(map[string]any{"tools": []Tool{}})})
}

// A server restart drops every session, and a server answers a request carrying
// a session it no longer holds with 404. The spec obliges the client to start a
// new session when it sees one. The client kept presenting the dead ID instead,
// so after any restart of an HTTP MCP server every tool call through the
// gateway failed for the rest of the process, while the server was up and
// answering and its tools stayed advertised.
func TestClientRenewsSessionTheServerNoLongerRecognises(t *testing.T) {
	srv := newRestartableMCPServer(t)
	c := NewClient(srv.URL, nil, 5*time.Second)
	ctx := context.Background()

	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := c.CallTool(ctx, "t", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool before restart: %v", err)
	}

	srv.restart()

	result, err := c.CallTool(ctx, "t", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool after the server restarted: %v — the client kept presenting a session the server had dropped", err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "ok" {
		t.Fatalf("result = %+v, want the tool's answer", result)
	}
	if got := srv.initializes.Load(); got != 2 {
		t.Errorf("initialize requests = %d, want 2 (the original handshake and one renewal)", got)
	}
	if got := srv.initWithSID.Load(); got != 0 {
		t.Errorf("%d initialize request(s) carried a session ID; a new session must be requested without one", got)
	}
	if got, _ := srv.lastCallerSID.Load().(string); got != "session-2" {
		t.Errorf("retried tool call carried session %q, want the renewed session-2", got)
	}

	// Later calls keep using the renewed session without another handshake.
	if _, err := c.CallTool(ctx, "t", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool on the renewed session: %v", err)
	}
	if got := srv.initializes.Load(); got != 2 {
		t.Errorf("initialize requests = %d after a healthy call, want still 2", got)
	}
}

// A server that numbers its sessions in memory issues the same ID again after a
// restart. The renewal decided whether the server still issued sessions by
// comparing the ID it held afterwards against the stale one, which cannot tell
// "issued the same ID again" from "issued none" — so it discarded the live
// session and sent every later call with none, refused 400 by a server that
// requires one, until the configuration was reloaded.
func TestClientRenewalKeepsAReissuedSessionID(t *testing.T) {
	srv := newRestartableMCPServer(t)
	srv.reissuesIDs = true
	c := NewClient(srv.URL, nil, 5*time.Second)
	ctx := context.Background()

	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := c.CallTool(ctx, "t", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool before restart: %v", err)
	}

	srv.restart()

	for i := range 2 {
		if _, err := c.CallTool(ctx, "t", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("CallTool %d after the server restarted and reissued session-1: %v", i, err)
		}
	}
	if got := c.getSessionID(); got != "session-1" {
		t.Errorf("session ID = %q, want the reissued session-1", got)
	}
	if got := srv.initializes.Load(); got != 2 {
		t.Errorf("initialize requests = %d, want 2 (the original handshake and one renewal)", got)
	}
}

// A server that issues no session ID on the renewal handshake runs without
// sessions now, and the expired ID must stop being presented to it.
func TestClientRenewalDropsTheSessionWhenNoneIsIssued(t *testing.T) {
	srv := newRestartableMCPServer(t)
	srv.issuesNoIDAfterRestart = true
	c := NewClient(srv.URL, nil, 5*time.Second)
	ctx := context.Background()

	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	srv.restart()

	if _, err := c.CallTool(ctx, "t", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool after the server stopped issuing sessions: %v", err)
	}
	if got, _ := srv.lastCallerSID.Load().(string); got != "" {
		t.Errorf("retried tool call carried session %q, want none", got)
	}
	if got := c.getSessionID(); got != "" {
		t.Errorf("session ID = %q, want none once the server issued none", got)
	}
}

// The retry happens once. A server that answers 404 whatever the session must
// fail the call, not loop through handshakes.
func TestClientSessionRenewalRetriesOnlyOnce(t *testing.T) {
	srv := newRestartableMCPServer(t)
	srv.toolCallsAlways404 = true
	c := NewClient(srv.URL, nil, 5*time.Second)
	ctx := context.Background()

	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := c.CallTool(ctx, "t", json.RawMessage(`{}`)); err == nil {
		t.Fatal("CallTool succeeded against a server that refuses every session")
	}
	if got := srv.toolCalls.Load(); got != 2 {
		t.Errorf("tool call attempts = %d, want 2 (the original and one retry)", got)
	}
	if got := srv.initializes.Load(); got != 2 {
		t.Errorf("initialize requests = %d, want 2 (the original handshake and one renewal)", got)
	}
}

// Concurrent calls that meet the same expired session share one renewal.
func TestClientConcurrentCallsShareOneSessionRenewal(t *testing.T) {
	srv := newRestartableMCPServer(t)
	c := NewClient(srv.URL, nil, 5*time.Second)
	ctx := context.Background()

	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	srv.restart()

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = c.CallTool(ctx, "t", json.RawMessage(`{}`))
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
	if got := srv.initializes.Load(); got != 2 {
		t.Errorf("initialize requests = %d, want 2 — concurrent calls must share one renewal", got)
	}
}
