package aigateway

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/mcp"
	providers "github.com/ferro-labs/ai-gateway/providers"
)

// The MCP tool-call audit hook is a Go-API field that no config file or JSON
// body can carry, so a config an embedder reloads from its file arrives
// without it. Rebuilding the MCP executor from that config dropped the hook,
// and every tool call after the reload went unaudited.
func TestReloadConfigKeepsTheMCPToolCallAuditHook(t *testing.T) {
	mcpSrv := newMCPTestServer(t)
	defer mcpSrv.Close()

	toolTurn := func(id string) *providers.Response {
		return &providers.Response{
			ID:    id,
			Model: "test-model",
			Choices: []providers.Choice{{
				Message: providers.Message{
					Role: "assistant",
					ToolCalls: []providers.ToolCall{{
						ID:       "tc-" + id,
						Type:     "function",
						Function: providers.FunctionCall{Name: "get_answer", Arguments: `{"q":"?"}`},
					}},
				},
				FinishReason: "tool_calls",
			}},
		}
	}
	answer := func(id string) *providers.Response {
		return &providers.Response{
			ID:      id,
			Model:   "test-model",
			Choices: []providers.Choice{{Message: providers.Message{Role: "assistant", Content: "42"}, FinishReason: "stop"}},
		}
	}
	mp := &multiCallProvider{
		name:      "test-provider",
		models:    []string{"test-model"},
		responses: []*providers.Response{toolTurn("1"), answer("2"), toolTurn("3"), answer("4")},
	}

	audited := make(chan mcp.ToolCallAuditEntry, 8)
	cfg := config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "test-provider"}},
		MCPServers: []mcp.ServerConfig{{
			Name:           "test-mcp",
			URL:            mcpSrv.URL + "/mcp",
			TimeoutSeconds: 5,
		}},
		MCPToolCallAuditFn: func(_ context.Context, entry mcp.ToolCallAuditEntry) {
			audited <- entry
		},
	}
	gw, err := newTestGateway(t, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw.RegisterProvider(mp)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	route := func(when string) {
		t.Helper()
		select {
		case <-gw.MCPInitDone():
		case <-ctx.Done():
			t.Fatalf("%s: MCP initialization did not finish", when)
		}
		if _, err := gw.Route(ctx, providers.Request{
			Model:    "test-model",
			Messages: []providers.Message{{Role: "user", Content: "What is the answer?"}},
		}); err != nil {
			t.Fatalf("%s: Route: %v", when, err)
		}
		select {
		case entry := <-audited:
			if entry.ToolName != "get_answer" || entry.Status != "ok" {
				t.Errorf("%s: audit entry = %+v, want an ok call to get_answer", when, entry)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: the tool call was not audited", when)
		}
	}

	route("before the reload")

	// The running config as a config file or JSON body would carry it.
	body, err := json.Marshal(gw.GetConfig())
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	var reloaded config.Config
	if err := json.Unmarshal(body, &reloaded); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if err := gw.ReloadConfig(ctx, reloaded); err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}

	route("after the reload")
	if got := mp.callCount(); got != 4 {
		t.Errorf("provider called %d times, want 4: one tool turn and one answer per request", got)
	}
}
