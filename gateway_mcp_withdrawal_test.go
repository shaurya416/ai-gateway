package aigateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/mcp"
	"github.com/ferro-labs/ai-gateway/providers"
)

// lookupMCPServer is an HTTP MCP server advertising one tool, "lookup". Once
// wedged it still accepts connections but answers nothing, which is what a hung
// server looks like from the gateway's side.
type lookupMCPServer struct {
	*httptest.Server
	wedged atomic.Bool
}

func newLookupMCPServer(t *testing.T) *lookupMCPServer {
	t.Helper()
	s := &lookupMCPServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var rpc struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if s.wedged.Load() {
			<-r.Context().Done()
			return
		}
		answer := func(result any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
		}
		switch rpc.Method {
		case "initialize":
			answer(map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "lookup", "version": "1"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			answer(map[string]any{"tools": []map[string]any{{
				"name": "lookup", "inputSchema": map[string]any{"type": "object"},
			}}})
		case "ping":
			answer(map[string]any{})
		default:
			answer(map[string]any{"content": []map[string]any{{"type": "text", "text": "found"}}})
		}
	}))
	return s
}

// lookupRetryingProvider asks for "lookup" on every turn until it has seen
// answerAfter tool results, then answers in prose. answerAfter 0 never answers.
// A model calling a tool again after it failed is the ordinary case.
type lookupRetryingProvider struct {
	answerAfter int
	calls       atomic.Int32
}

func (p *lookupRetryingProvider) Name() string                  { return "lookup-model" }
func (p *lookupRetryingProvider) ConfiguredModels() []string    { return []string{"lookup-model"} }
func (p *lookupRetryingProvider) SupportsModel(m string) bool   { return m == "lookup-model" }
func (p *lookupRetryingProvider) Models() []providers.ModelInfo { return nil }
func (p *lookupRetryingProvider) Complete(_ context.Context, req providers.Request) (*providers.Response, error) {
	p.calls.Add(1)
	seen := 0
	for _, m := range req.Messages {
		if m.Role == "tool" {
			seen++
		}
	}
	if p.answerAfter > 0 && seen >= p.answerAfter {
		return &providers.Response{ID: "answer", Model: "lookup-model", Choices: []providers.Choice{{
			Message:      providers.Message{Role: "assistant", Content: "could not look it up"},
			FinishReason: "stop",
		}}}, nil
	}
	return &providers.Response{ID: "tool-turn", Model: "lookup-model", Choices: []providers.Choice{{
		Message: providers.Message{Role: "assistant", ToolCalls: []providers.ToolCall{{
			ID: "tc", Type: "function", Function: providers.FunctionCall{Name: "lookup", Arguments: "{}"},
		}}},
		FinishReason: "tool_calls",
	}}}, nil
}

// newLookupGateway builds a gateway whose one MCP server is srv and waits for
// its handshake.
func newLookupGateway(t *testing.T, srv *lookupMCPServer, p *lookupRetryingProvider, requestTimeout string, mcpTimeoutSeconds int) *Gateway {
	t.Helper()
	gw, err := newTestGateway(t, config.Config{
		Strategy:       config.StrategyConfig{Mode: config.ModeSingle},
		Targets:        []config.Target{{VirtualKey: "lookup-model"}},
		RequestTimeout: requestTimeout,
		MCPServers: []mcp.ServerConfig{{
			Name: "tools", URL: srv.URL, TimeoutSeconds: mcpTimeoutSeconds, Required: true,
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw.RegisterProvider(p)
	select {
	case <-gw.MCPInitDone():
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the MCP handshake")
	}
	if !liveMCPRegistry(gw).IsReady("tools") {
		t.Fatalf("precondition: the MCP server did not complete its handshake: %+v", liveMCPRegistry(gw).Status())
	}
	return gw
}

// liveMCPRegistry returns the registry the gateway is currently serving from.
func liveMCPRegistry(g *Gateway) *mcp.Registry {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.mcpRegistry
}

// TestRoute_MCPLoopKeepsOwningToolsOfAWithdrawnServer is the regression test for
// a server withdrawn partway through an agentic loop handing the client a
// tool_calls turn for a tool it never declared.
//
// The request is offered the server's tools once, when it starts. The first
// call to a server that has gone withdraws it, but the model still holds the
// tool and asks for it again — and the loop decided what it owned by asking the
// registry, which no longer resolved the name. So it stopped and returned the
// model's turn as it stood: finish_reason tool_calls, naming a tool the gateway
// injected and its arguments. The depth-limit guard asked the same question and
// let the same turn through. Every tool the request was offered is now the
// gateway's to answer for the whole loop, and a call to one whose server has
// gone is answered with an error the model can read.
func TestRoute_MCPLoopKeepsOwningToolsOfAWithdrawnServer(t *testing.T) {
	for _, tc := range []struct {
		name        string
		answerAfter int
		wantFinish  string
		wantCalls   int32
	}{
		// The model gives up after its second failed call and answers.
		{"model answers after retrying", 2, "stop", 3},
		// The model never stops asking: the depth limit ends the loop.
		{"model retries to the depth limit", 0, "length", 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newLookupMCPServer(t)
			p := &lookupRetryingProvider{answerAfter: tc.answerAfter}
			gw := newLookupGateway(t, srv, p, "", 2)
			srv.Close() // the server goes away after its handshake

			resp, err := gw.Route(t.Context(), providers.Request{
				Model:    "lookup-model",
				Messages: []providers.Message{{Role: "user", Content: "look it up"}},
			})
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if liveMCPRegistry(gw).IsReady("tools") {
				t.Fatal("precondition: the server that went away was not withdrawn")
			}
			ch := resp.Choices[0]
			if len(ch.Message.ToolCalls) > 0 {
				t.Fatalf("the client was handed tool_calls for %q, a tool the gateway injected and the client never declared (finish_reason %q)",
					ch.Message.ToolCalls[0].Function.Name, ch.FinishReason)
			}
			if ch.FinishReason != tc.wantFinish {
				t.Errorf("finish_reason = %q, want %q", ch.FinishReason, tc.wantFinish)
			}
			if got := p.calls.Load(); got != tc.wantCalls {
				t.Errorf("provider called %d times, want %d: every retried call must be answered and the loop continued", got, tc.wantCalls)
			}
		})
	}
}

// TestRoute_RequestTimeoutOnAHungMCPServerWithdrawsIt is the regression test for
// the gateway's own request_timeout shielding a hung HTTP MCP server from
// withdrawal.
//
// A tool call starts only after the first model turn, so with request_timeout at
// or below the server's per-call timeout the request's deadline is always what
// ends a call to a server that accepts connections and never answers. That
// deadline was read as the caller having given up, so no call was ever
// confirmed: the server stayed ready, a required one held /readyz at 200, and
// every request that offered its tools failed. Only a caller that hung up is
// not evidence now; the request's deadline is, and confirming it does not hold
// the failed request past that deadline.
func TestRoute_RequestTimeoutOnAHungMCPServerWithdrawsIt(t *testing.T) {
	srv := newLookupMCPServer(t)
	defer srv.Close()
	p := &lookupRetryingProvider{answerAfter: 1}
	gw := newLookupGateway(t, srv, p, "300ms", 2)
	srv.wedged.Store(true)

	start := time.Now()
	_, err := gw.Route(t.Context(), providers.Request{
		Model:    "lookup-model",
		Messages: []providers.Message{{Role: "user", Content: "look it up"}},
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Route succeeded against a server that answers nothing; want the request deadline")
	}
	// The confirming ping waits up to the server's 2s timeout. The request must
	// not wait with it.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("Route took %s with a 300ms request_timeout; confirming the failure held the request past its deadline", elapsed)
	}

	reg := liveMCPRegistry(gw)
	deadline := time.Now().Add(10 * time.Second)
	for reg.IsReady("tools") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if reg.IsReady("tools") {
		t.Fatalf("a hung server whose calls end on request_timeout is still ready: %+v", reg.Status())
	}
	if st := reg.Status(); len(st) != 1 || !strings.Contains(st[0].LastError, "unreachable") {
		t.Errorf("Status() = %+v; want the server withdrawn as unreachable", st)
	}
}
