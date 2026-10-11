package httpserver_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
	"github.com/ferro-labs/ai-gateway/internal/httpserver"
	"github.com/ferro-labs/ai-gateway/mcp"
	"github.com/ferro-labs/ai-gateway/providers"
)

// restartableMCPServer is an HTTP MCP server advertising one tool, "lookup",
// that can stop and come back on the same address.
type restartableMCPServer struct {
	t     *testing.T
	addr  string
	mu    sync.Mutex
	srv   *httptest.Server
	calls atomic.Int32
}

func startRestartableMCPServer(t *testing.T) *restartableMCPServer {
	t.Helper()
	s := &restartableMCPServer{t: t}
	s.listen("127.0.0.1:0")
	t.Cleanup(s.stop)
	return s
}

func (s *restartableMCPServer) url() string { return "http://" + s.addr + "/mcp" }

func (s *restartableMCPServer) listen(addr string) {
	s.t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(s.t.Context(), "tcp", addr)
	if err != nil {
		s.t.Fatalf("listen on %s: %v", addr, err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	_ = srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	s.mu.Lock()
	s.srv = srv
	if s.addr == "" {
		s.addr = l.Addr().String()
	}
	s.mu.Unlock()
}

func (s *restartableMCPServer) stop() {
	s.mu.Lock()
	srv := s.srv
	s.srv = nil
	s.mu.Unlock()
	if srv != nil {
		srv.Close()
	}
}

func (s *restartableMCPServer) revive() { s.listen(s.addr) }

func (s *restartableMCPServer) serve(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodGet:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID     any    `json:"id"`
		Method string `json:"method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	answer := func(result any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "sid-restartable")
		answer(map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "restartable", "version": "1"},
		})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		answer(map[string]any{"tools": []map[string]any{{
			"name": "lookup", "inputSchema": map[string]any{"type": "object"},
		}}})
	case "ping":
		answer(map[string]any{})
	case "tools/call":
		s.calls.Add(1)
		answer(map[string]any{"content": []map[string]any{{"type": "text", "text": "found"}}})
	default:
		http.Error(w, "unknown method", http.StatusBadRequest)
	}
}

// lookupProvider asks for the "lookup" tool until it has been handed a tool
// result, then answers.
type lookupProvider struct{}

func (lookupProvider) Name() string                  { return "mcp-loop" }
func (lookupProvider) ConfiguredModels() []string    { return []string{"mcp-loop-model"} }
func (lookupProvider) SupportsModel(m string) bool   { return m == "mcp-loop-model" }
func (lookupProvider) Models() []providers.ModelInfo { return nil }
func (lookupProvider) Complete(_ context.Context, req providers.Request) (*providers.Response, error) {
	if last := req.Messages[len(req.Messages)-1]; last.Role == "tool" {
		return &providers.Response{
			ID: "final", Model: "mcp-loop-model",
			Choices: []providers.Choice{{
				Message:      providers.Message{Role: "assistant", Content: "done"},
				FinishReason: "stop",
			}},
		}, nil
	}
	return &providers.Response{
		ID: "call", Model: "mcp-loop-model",
		Choices: []providers.Choice{{
			Message: providers.Message{
				Role: "assistant",
				ToolCalls: []providers.ToolCall{{
					ID: "tc-lookup", Type: "function",
					Function: providers.FunctionCall{Name: "lookup", Arguments: "{}"},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}, nil
}

// TestReadyzFollowsARequiredHTTPMCPServerThatDiesAndRecovers pins readiness
// for a required HTTP MCP server across its death and its recovery, through
// the production wiring.
//
// Death after the handshake was detected for stdio servers only, so a required
// HTTP server that went away left /readyz answering 200 for an instance that
// could not serve what it was configured to require. Detection follows a
// failed call — a request whose model asks for the tool is what notices — and
// recovery is the gateway's own: no reload, no restart.
func TestReadyzFollowsARequiredHTTPMCPServerThatDiesAndRecovers(t *testing.T) {
	srv := startRestartableMCPServer(t)
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "mcp-loop"}},
		MCPServers: []mcp.ServerConfig{{
			Name: "required-tools", URL: srv.url(), TimeoutSeconds: 2, Required: true,
		}},
	})
	if err != nil {
		t.Fatalf("New gateway: %v", err)
	}
	gw.RegisterProvider(lookupProvider{})
	select {
	case <-gw.MCPInitDone():
	case <-time.After(30 * time.Second):
		t.Fatal("MCP initialization did not finish")
	}

	reg := providers.NewRegistry()
	reg.Register(lookupProvider{})
	router := httpserver.NewRouter(reg, repository.NewKeyStore(), nil, nil, gw, nil, nil, nil, nil, nil, "", nil)
	readyz := func() (int, string) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var body struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body.Reason
	}
	// /readyz caches its answer for a second, so each change is polled for.
	awaitReadyz := func(want int, within time.Duration) string {
		t.Helper()
		deadline := time.Now().Add(within)
		for {
			code, reason := readyz()
			if code == want {
				return reason
			}
			if time.Now().After(deadline) {
				t.Fatalf("GET /readyz = %d (%q); want %d within %s", code, reason, want, within)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	route := func() {
		t.Helper()
		resp, err := gw.Route(t.Context(), providers.Request{
			Model:    "mcp-loop-model",
			Messages: []providers.Message{{Role: "user", Content: "look it up"}},
		})
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		if resp.ID != "final" {
			t.Fatalf("Route answered %q; want the loop to finish", resp.ID)
		}
	}

	awaitReadyz(http.StatusOK, 5*time.Second)
	route()
	if srv.calls.Load() != 1 {
		t.Fatalf("precondition: the tool call did not reach the MCP server")
	}

	srv.stop()
	route()
	if reason := awaitReadyz(http.StatusServiceUnavailable, 5*time.Second); reason != "required mcp server unavailable" {
		t.Errorf("GET /readyz reason = %q; want %q", reason, "required mcp server unavailable")
	}

	srv.revive()
	awaitReadyz(http.StatusOK, 20*time.Second)
	route()
	if got := srv.calls.Load(); got != 2 {
		t.Errorf("the MCP server received %d tool calls; want the one after recovery to reach it", got)
	}
}
