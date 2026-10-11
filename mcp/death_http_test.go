package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// revivableTool is the one tool revivableMCPServer advertises.
const revivableTool = "echo"

// revivableMCPServer is an HTTP MCP server that can stop and come back on the
// same address — what a restart, a redeploy or a network partition looks like
// from the gateway's side of the connection.
type revivableMCPServer struct {
	t    *testing.T
	addr string

	mu  sync.Mutex
	srv *httptest.Server

	// toolStatus, when non-zero, is the HTTP status tools/call is refused with.
	toolStatus atomic.Int32
	// toolRPCError answers tools/call with a JSON-RPC error.
	toolRPCError atomic.Bool
	// toolIsError answers tools/call with a result that sets isError.
	toolIsError atomic.Bool
	// wedged holds every tools/call and ping open until the client gives up on
	// it: a server that accepts connections and answers nothing.
	wedged atomic.Bool
	// hangInitialize holds initialize open until the client gives up on it;
	// initializeHeld is signalled once when it starts doing so.
	hangInitialize atomic.Bool
	initializeHeld chan struct{}
	heldOnce       sync.Once

	pings atomic.Int32
	calls atomic.Int32
}

// newRevivableMCPServer starts the server on a free loopback port. Callers stop
// it themselves (stop), so a test can order its teardown ahead of a leak check.
func newRevivableMCPServer(t *testing.T) *revivableMCPServer {
	t.Helper()
	s := &revivableMCPServer{t: t, initializeHeld: make(chan struct{})}
	s.listen("127.0.0.1:0")
	return s
}

func (s *revivableMCPServer) url() string { return "http://" + s.addr + "/mcp" }

// listen serves on addr; the first call records the address it got.
func (s *revivableMCPServer) listen(addr string) {
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

// stop takes the server down: the listener closes, so a new connection is
// refused, and every open connection is closed with it.
func (s *revivableMCPServer) stop() {
	s.mu.Lock()
	srv := s.srv
	s.srv = nil
	s.mu.Unlock()
	if srv != nil {
		srv.Close()
	}
}

// revive brings the server back on the address it had.
func (s *revivableMCPServer) revive() { s.listen(s.addr) }

func (s *revivableMCPServer) serve(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodGet:
		// No event stream: nothing here sends the client requests.
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	answer := func(result any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: mustMarshal(result)})
	}
	switch req.Method {
	case mcpMethodInitialize:
		if s.hangInitialize.Load() {
			s.heldOnce.Do(func() { close(s.initializeHeld) })
			<-r.Context().Done()
			return
		}
		w.Header().Set("Mcp-Session-Id", "sid-revivable")
		answer(map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "revivable", "version": "1"},
		})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case mcpMethodToolsList:
		answer(map[string]any{"tools": []Tool{{Name: revivableTool, InputSchema: json.RawMessage(`{"type":"object"}`)}}})
	case mcpMethodPing:
		s.pings.Add(1)
		if s.wedged.Load() {
			<-r.Context().Done()
			return
		}
		answer(map[string]any{})
	case mcpMethodToolsCall:
		s.calls.Add(1)
		switch {
		case s.wedged.Load():
			<-r.Context().Done()
		case s.toolStatus.Load() != 0:
			http.Error(w, "refused", int(s.toolStatus.Load()))
		case s.toolRPCError.Load():
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32602, "message": "invalid params"},
			})
		case s.toolIsError.Load():
			answer(map[string]any{"content": []map[string]any{{"type": "text", "text": "bad input"}}, "isError": true})
		default:
			answer(map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}})
		}
	default:
		http.Error(w, "unknown method", http.StatusBadRequest)
	}
}

// readyHTTPRegistry registers s under name with re-admission spaced by delay,
// and completes its handshake.
func readyHTTPRegistry(t *testing.T, s *revivableMCPServer, name string, delay time.Duration, timeoutSeconds int) *Registry {
	t.Helper()
	reg := NewRegistry(nil)
	reg.readmitDelay, reg.readmitDelayCap = delay, 2*delay
	reg.RegisterConfig(ServerConfig{Name: name, URL: s.url(), TimeoutSeconds: timeoutSeconds})
	reg.InitializeAll(t.Context(), nil)
	if !reg.IsReady(name) {
		t.Fatalf("precondition: the server did not complete its handshake: %+v", reg.Status())
	}
	return reg
}

// httpClientOf returns the HTTP client the registry built for name.
func httpClientOf(t *testing.T, reg *Registry, name string) *Client {
	t.Helper()
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	c, ok := reg.servers[name].client.(*Client)
	if !ok {
		t.Fatalf("server %q is not served by the HTTP client", name)
	}
	return c
}

// TestHTTPServerThatDiesIsWithdrawnAndRestored is the regression test for an
// HTTP MCP server that stopped answering after its handshake staying ready.
//
// Death after the handshake was detected for stdio servers only. An HTTP
// server that went away kept its tools advertised, so every turn offered the
// model tools whose every call failed, and a server marked required held
// /readyz at 200 for an instance that could not serve what it was required for.
// The withdrawal used to be terminal as well — nothing re-initialized a server
// once withdrawn — which is why an HTTP server was never withdrawn at all.
//
// Now an unanswered call that a ping confirms withdraws the server, and it is
// re-admitted, tools and gauge included, once it completes a handshake again.
func TestHTTPServerThatDiesIsWithdrawnAndRestored(t *testing.T) {
	s := newRevivableMCPServer(t)
	defer s.stop()
	const name = "revives"
	reg := readyHTTPRegistry(t, s, name, 50*time.Millisecond, 2)
	defer func() { _ = reg.Close() }()
	exec := NewExecutor(reg, 5, nil)

	s.stop()
	msg := exec.executeToolCall(t.Context(), toolCallNamed(revivableTool))
	if !strings.Contains(msg.Content, "error") {
		t.Fatalf("a call to a server that is down answered %q; want an error payload", msg.Content)
	}

	if reg.IsReady(name) {
		t.Fatal("an HTTP server that stopped answering is still ready")
	}
	if tools := reg.AllTools(); len(tools) != 0 {
		t.Errorf("AllTools still advertises %v for a server that is down", tools)
	}
	if _, ok := reg.FindToolServer(revivableTool); ok {
		t.Error("FindToolServer still resolves a tool on a server that is down")
	}
	if st := reg.Status(); len(st) != 1 || st[0].Ready || st[0].LastError == "" {
		t.Errorf("Status() = %+v; want the server unready with the reason recorded", st)
	}
	if v, ok := serverUpSeries(t, name); !ok || v != 0 {
		t.Errorf("gateway_mcp_server_up = %v (present %v); want 0 while the server is down", v, ok)
	}

	// Re-admission keeps trying while the server stays down, and records why
	// each attempt failed.
	time.Sleep(200 * time.Millisecond)
	if reg.IsReady(name) {
		t.Fatal("a server that is still down was re-admitted")
	}

	s.revive()
	waitUntil(t, 10*time.Second, "the server to be re-admitted once it answers again", func() bool {
		return reg.IsReady(name)
	})

	if tools := reg.AllTools(); len(tools) != 1 || tools[0].Name != revivableTool {
		t.Errorf("AllTools = %v after recovery; want the server's tool advertised again", tools)
	}
	if st := reg.Status(); st[0].LastError != "" {
		t.Errorf("Status() still reports %q for a server that recovered", st[0].LastError)
	}
	if v, _ := serverUpSeries(t, name); v != 1 {
		t.Errorf("gateway_mcp_server_up = %v after recovery; want 1", v)
	}
	before := s.calls.Load()
	if msg := exec.executeToolCall(t.Context(), toolCallNamed(revivableTool)); msg.Content != "ok" {
		t.Errorf("a call after recovery answered %q; want the tool's result", msg.Content)
	}
	if s.calls.Load() != before+1 {
		t.Error("the call after recovery never reached the server")
	}
}

// TestOfferedToolsStayOwnedAfterTheirServerIsWithdrawn pins the executor half of
// a server withdrawn mid-loop. The request was offered the tool, so the model
// may ask for it again; the loop must keep answering that call rather than hand
// it to a client that never declared the tool — at every depth, the limit
// included. Ownership widens only to what was offered: a caller's own tool is
// still the caller's.
func TestOfferedToolsStayOwnedAfterTheirServerIsWithdrawn(t *testing.T) {
	s := newRevivableMCPServer(t)
	reg := readyHTTPRegistry(t, s, "gone", time.Hour, 2)
	defer func() { _ = reg.Close() }()
	shared := NewExecutor(reg, 3, nil)
	exec := shared.WithOfferedTools(reg.AllTools())

	s.stop()
	exec.executeToolCall(t.Context(), toolCallNamed(revivableTool))
	if reg.IsReady("gone") {
		t.Fatal("precondition: the server was not withdrawn")
	}

	if !exec.ShouldContinueLoop(toolCallResponse(revivableTool), 1) {
		t.Fatal("the loop stopped on a call to a tool the request was offered; the client would be handed a tool it never declared")
	}
	extra, err := exec.ResolvePendingToolCalls(t.Context(), toolCallResponse(revivableTool))
	if err != nil {
		t.Fatalf("ResolvePendingToolCalls: %v", err)
	}
	assertProtocolComplete(t, extra)
	if len(extra) != 2 || !strings.Contains(extra[1].Content, "error") {
		t.Errorf("the call to a withdrawn server's tool was answered with %+v; want one error result the model can read", extra)
	}

	atLimit := toolCallResponse(revivableTool)
	atLimit.Choices[0].FinishReason = core.FinishReasonToolCalls
	if exec.ShouldContinueLoop(atLimit, 3) {
		t.Fatal("the loop continued past the depth limit")
	}
	if len(atLimit.Choices[0].Message.ToolCalls) != 0 || atLimit.Choices[0].FinishReason != core.FinishReasonLength {
		t.Errorf("at the depth limit the client was handed %+v; want the offered tool's call dropped and the answer marked truncated", atLimit.Choices[0])
	}

	if exec.ShouldContinueLoop(toolCallResponse("caller_tool"), 1) {
		t.Error("an executor scoped to its offered tools claimed a tool it was never offered")
	}
	if shared.ShouldContinueLoop(toolCallResponse(revivableTool), 1) {
		t.Error("the shared executor owns a withdrawn server's tool; only a request it was offered to should")
	}
}

// TestHTTPAnsweredToolErrorsDoNotWithdraw pins what is not evidence. A server
// that answered — with an HTTP error status, a JSON-RPC error, or a result that
// sets isError — is there to answer, so none of these withdraws it, and none is
// even worth a confirming ping.
func TestHTTPAnsweredToolErrorsDoNotWithdraw(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(*revivableMCPServer)
	}{
		{"http 400", func(s *revivableMCPServer) { s.toolStatus.Store(http.StatusBadRequest) }},
		{"http 403", func(s *revivableMCPServer) { s.toolStatus.Store(http.StatusForbidden) }},
		{"http 500", func(s *revivableMCPServer) { s.toolStatus.Store(http.StatusInternalServerError) }},
		{"json-rpc error", func(s *revivableMCPServer) { s.toolRPCError.Store(true) }},
		{"tool isError result", func(s *revivableMCPServer) { s.toolIsError.Store(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRevivableMCPServer(t)
			defer s.stop()
			reg := readyHTTPRegistry(t, s, "answers", 10*time.Millisecond, 2)
			defer func() { _ = reg.Close() }()
			exec := NewExecutor(reg, 5, nil)

			tc.arm(s)
			exec.executeToolCall(t.Context(), toolCallNamed(revivableTool))
			exec.executeToolCall(t.Context(), toolCallNamed(revivableTool))

			if !reg.IsReady("answers") {
				t.Fatalf("a server that answered the call was withdrawn: %+v", reg.Status())
			}
			if len(reg.AllTools()) != 1 {
				t.Errorf("AllTools = %v; want the tool still advertised", reg.AllTools())
			}
			if got := s.calls.Load(); got != 2 {
				t.Errorf("the server received %d calls; want both sent", got)
			}
			if got := s.pings.Load(); got != 0 {
				t.Errorf("the server was pinged %d times; an answered call is not evidence it has gone", got)
			}
		})
	}
}

// TestHTTPBrokenPipeKeepsServerReady pins the case confirmation exists for: an
// unanswered call to a server that is still there.
//
// net/http reports a connection the server reset mid-upload — a server
// restarting, or a proxy refusing a large body — as a write that failed with
// EPIPE in a share of cases. EPIPE is conclusive for a stdio pipe, and reading
// it the same way for HTTP withdrew a server whose next request, on a new
// connection, would have been served. The ping that confirms the failure is
// that next request: the server answers it, and stays ready.
func TestHTTPBrokenPipeKeepsServerReady(t *testing.T) {
	s := newRevivableMCPServer(t)
	defer s.stop()
	reg := readyHTTPRegistry(t, s, "remote", 10*time.Millisecond, 5)
	defer func() { _ = reg.Close() }()

	// Fail the next request the way net/http reports that reset, then serve
	// normally.
	c := httpClientOf(t, reg, "remote")
	base := c.httpClient.Transport
	var armed atomic.Bool
	c.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if armed.CompareAndSwap(true, false) {
			return nil, &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)}
		}
		return base.RoundTrip(r)
	})

	// The failure must be one the stdio rule reads as a dead transport, or this
	// test proves nothing about it.
	armed.Store(true)
	if _, err := c.CallTool(t.Context(), revivableTool, nil); !isTransportDead(err) || !isUnreachable(err) {
		t.Fatalf("precondition: CallTool error %v is not an unanswered broken pipe", err)
	}

	exec := NewExecutor(reg, 5, nil)
	armed.Store(true)
	exec.executeToolCall(t.Context(), toolCallNamed(revivableTool))

	if !reg.IsReady("remote") {
		t.Fatalf("one broken connection withdrew an HTTP server that was answering: %+v", reg.Status())
	}
	if got := s.pings.Load(); got != 1 {
		t.Errorf("the server was pinged %d times; want the failure confirmed once", got)
	}
	if len(reg.AllTools()) != 1 {
		t.Errorf("AllTools = %v after one broken connection; want the server's tool still advertised", reg.AllTools())
	}
	before := s.calls.Load()
	exec.executeToolCall(t.Context(), toolCallNamed(revivableTool))
	if s.calls.Load() != before+1 {
		t.Error("the call after the broken connection was not sent to the server")
	}
}

// TestHTTPCallerCancellationIsNotEvidence covers a request the caller abandoned:
// the call goes unanswered because the client hung up, which says nothing about
// the server, so it is not confirmed and does not withdraw anything.
func TestHTTPCallerCancellationIsNotEvidence(t *testing.T) {
	s := newRevivableMCPServer(t)
	defer s.stop()
	reg := readyHTTPRegistry(t, s, "remote", 10*time.Millisecond, 5)
	defer func() { _ = reg.Close() }()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	NewExecutor(reg, 5, nil).executeToolCall(ctx, toolCallNamed(revivableTool))

	if !reg.IsReady("remote") {
		t.Fatalf("a call the caller cancelled withdrew the server: %+v", reg.Status())
	}
	if got := s.pings.Load(); got != 0 {
		t.Errorf("the server was pinged %d times over a call the caller cancelled", got)
	}
}

// TestHTTPRequestDeadlineIsEvidence is the regression test for a request's own
// deadline shielding a hung server from withdrawal.
//
// A server that accepts connections and never answers fails each call when a
// deadline runs out — the server's per-call timeout, or the request's, whichever
// is shorter. The request's deadline was read as the caller having given up, so
// with a request deadline at or below the per-call timeout — the gateway's
// request_timeout and the default 30s per-call timeout, say — no call was ever
// confirmed and the server stayed ready for good. A deadline elapsing while the
// server keeps silent is evidence like any other unanswered request; only a
// caller that hung up is not.
//
// Confirming it must not hold the request past its deadline either: the ping
// waits on the same silent server, so it carries on after the call returns.
func TestHTTPRequestDeadlineIsEvidence(t *testing.T) {
	s := newRevivableMCPServer(t)
	defer s.stop()
	const name = "wedged"
	// Re-admission an hour out: this test is about withdrawal alone.
	reg := readyHTTPRegistry(t, s, name, time.Hour, 2)
	defer func() { _ = reg.Close() }()
	s.wedged.Store(true)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	msg := NewExecutor(reg, 5, nil).executeToolCall(ctx, toolCallNamed(revivableTool))
	elapsed := time.Since(start)
	if !strings.Contains(msg.Content, "error") {
		t.Fatalf("a call to a wedged server answered %q; want an error payload", msg.Content)
	}
	// The ping waits up to the server's 2s timeout; the call must not wait with it.
	if elapsed > time.Second {
		t.Errorf("the call took %s on a 100ms request deadline; the confirming ping held it past its deadline", elapsed)
	}

	waitUntil(t, 10*time.Second, "the wedged server to be withdrawn", func() bool { return !reg.IsReady(name) })
	if got := s.pings.Load(); got != 1 {
		t.Errorf("the server was pinged %d times; want the failed call confirmed once", got)
	}
	if st := reg.Status(); len(st) != 1 || !strings.Contains(st[0].LastError, errServerUnreachable.Error()) {
		t.Errorf("Status() = %+v; want the server withdrawn as unreachable", st)
	}
}

// TestIsUnreachableOnlyForUnansweredRequests pins the classification at the
// client, where the distinction is made: a request with no answer is
// unreachable, and anything the server sent back is not.
func TestIsUnreachableOnlyForUnansweredRequests(t *testing.T) {
	s := newRevivableMCPServer(t)
	c := NewClient(s.url(), nil, 2*time.Second)
	defer func() { _ = c.Close() }()
	if _, err := c.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	s.toolStatus.Store(http.StatusUnauthorized)
	if _, err := c.CallTool(t.Context(), revivableTool, nil); err == nil || isUnreachable(err) {
		t.Errorf("an HTTP 401 answer classified as unreachable (err %v)", err)
	}
	s.toolStatus.Store(0)
	s.toolRPCError.Store(true)
	if _, err := c.CallTool(t.Context(), revivableTool, nil); err == nil || isUnreachable(err) {
		t.Errorf("a JSON-RPC error answer classified as unreachable (err %v)", err)
	}
	if err := c.Ping(t.Context()); err != nil {
		t.Errorf("ping to a live server: %v", err)
	}

	s.stop()
	if _, err := c.CallTool(t.Context(), revivableTool, nil); !isUnreachable(err) {
		t.Errorf("a call to a closed port classified as answered (err %v)", err)
	}
	if err := c.Ping(t.Context()); !isUnreachable(err) {
		t.Errorf("a ping to a closed port classified as answered (err %v)", err)
	}

	// The resolver's answer is stubbed: what a lookup of a real name returns
	// depends on the machine running the test.
	dnsFail := NewClient("http://mcp.invalid/mcp", nil, 2*time.Second)
	dnsFail.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "mcp.invalid", IsNotFound: true}}
	})
	if _, err := dnsFail.CallTool(t.Context(), revivableTool, nil); !isUnreachable(err) {
		t.Errorf("a call to a host that does not resolve classified as answered (err %v)", err)
	}

	// The wrap keeps the chain beneath it: a deadline is still a deadline.
	wrapped := &unreachableError{context.DeadlineExceeded}
	if !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Error("unreachableError hid the error it wraps from errors.Is")
	}
}

// TestRegistryCloseStopsReadmission covers teardown while a withdrawn server is
// still down: the re-admission loop must end with the registry, leaving no
// goroutine behind.
//
// The wait between attempts is an hour, far longer than the bound on Close, so
// only teardown cancelling that wait can end it in time. With a wait of a few
// milliseconds a loop that ignored teardown during its backoff still ended
// within one tick — its next attempt found the client detached — and this test
// passed, while in production the same loop held Close for the whole backoff,
// up to two minutes, well past Gateway.Close's shutdown budget.
func TestRegistryCloseStopsReadmission(t *testing.T) {
	ignore := goleak.IgnoreCurrent()

	s := newRevivableMCPServer(t)
	reg := readyHTTPRegistry(t, s, "gone", time.Hour, 2)
	s.stop()
	NewExecutor(reg, 5, nil).executeToolCall(t.Context(), toolCallNamed(revivableTool))
	if reg.IsReady("gone") {
		t.Fatal("precondition: the server was not withdrawn")
	}

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		if err := reg.Close(); err != nil {
			t.Logf("close: %v", err)
		}
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close is still waiting after 1s with a re-admission loop in its backoff; want the wait cut short")
	}
	goleak.VerifyNone(t, ignore)
}

// TestRegistryCloseCancelsAReadmissionHandshake covers teardown while a
// re-admission handshake is in flight against a server that accepts the
// connection and never answers. Close must cancel it rather than wait out the
// handshake's own timeout, and leave nothing running.
func TestRegistryCloseCancelsAReadmissionHandshake(t *testing.T) {
	ignore := goleak.IgnoreCurrent()

	s := newRevivableMCPServer(t)
	reg := readyHTTPRegistry(t, s, "wedged", 10*time.Millisecond, 30)
	s.stop()
	NewExecutor(reg, 5, nil).executeToolCall(t.Context(), toolCallNamed(revivableTool))
	if reg.IsReady("wedged") {
		t.Fatal("precondition: the server was not withdrawn")
	}

	s.hangInitialize.Store(true)
	s.revive()
	select {
	case <-s.initializeHeld:
	case <-time.After(10 * time.Second):
		t.Fatal("no re-admission handshake reached the server")
	}

	start := time.Now()
	if err := reg.Close(); err != nil {
		t.Logf("close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Close took %s with a handshake in flight; want it cancelled", elapsed)
	}
	if reg.IsReady("wedged") {
		t.Error("a handshake cancelled by Close re-admitted the server")
	}
	s.stop()
	goleak.VerifyNone(t, ignore)
}

// TestReadmissionEndsWhenTheServerIsReplaced covers a reload that replaces a
// withdrawn server under the same name: the loop serving the old transport
// ends, and never touches the replacement.
func TestReadmissionEndsWhenTheServerIsReplaced(t *testing.T) {
	s := newRevivableMCPServer(t)
	defer s.stop()
	reg := readyHTTPRegistry(t, s, "svc", 10*time.Millisecond, 2)
	defer func() { _ = reg.Close() }()
	old := httpClientOf(t, reg, "svc")

	s.stop()
	NewExecutor(reg, 5, nil).executeToolCall(t.Context(), toolCallNamed(revivableTool))
	if reg.IsReady("svc") {
		t.Fatal("precondition: the server was not withdrawn")
	}

	// Replace it with a server that points nowhere, so only the old loop could
	// ever mark the name ready.
	reg.RegisterConfig(ServerConfig{Name: "svc", URL: "http://127.0.0.1:1/mcp", TimeoutSeconds: 1})
	s.revive()

	done := make(chan struct{})
	go func() {
		reg.readmitting.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the re-admission loop for a replaced transport is still running")
	}
	if reg.IsReady("svc") {
		t.Error("the old transport's re-admission marked the replacement ready")
	}
	if httpClientOf(t, reg, "svc") == old {
		t.Error("precondition: re-registration should have installed a new client")
	}
}
