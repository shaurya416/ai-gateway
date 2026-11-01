package aigateway

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/mcp"
	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

// reloadPlanProvider answers chat for "m" as itself.
func reloadPlanProvider(name string) *mockProvider {
	return &mockProvider{
		name:   name,
		models: []string{"m"},
		resp: &providers.Response{
			Model:   "m",
			Choices: []providers.Choice{{Message: providers.Message{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
		},
	}
}

// A chat request that started under one config and reaches the provider walk
// after a reload is walked by the strategy it started with, under that
// strategy's own mode. The strategy is fetched before the before_request stage
// and the mode was read again afterwards, so a reload from fallback over [a, b]
// to single over [b] handed the request the fallback order [a, b] with single's
// rule of attempting only the head — a, which the new config no longer targets
// — and it was answered 404 model_not_found for a model both configs serve.
func TestRoute_ReloadMidRequestWalksTheStrategyItStartedWith(t *testing.T) {
	before := config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeFallback},
		Targets:  []config.Target{{VirtualKey: "a"}, {VirtualKey: "b"}},
	}
	after := config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "b"}},
	}

	gw, err := newTestGateway(t, before)
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	gw.RegisterProvider(reloadPlanProvider("a"))
	gw.RegisterProvider(reloadPlanProvider("b"))

	entered := make(chan struct{})
	release := make(chan struct{})
	if err := gw.RegisterPlugin(plugin.StageBeforeRequest, &testPlugin{
		name: "hold",
		typ:  plugin.TypeGuardrail,
		execFn: func(ctx context.Context, _ *plugin.Context) error {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		},
	}); err != nil {
		t.Fatalf("register plugin: %v", err)
	}

	type result struct {
		resp *providers.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := gw.Route(context.Background(), providers.Request{
			Model:    "m",
			Messages: []providers.Message{{Role: "user", Content: "hi"}},
		})
		done <- result{resp, err}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the before_request stage")
	}
	if err := gw.ReloadConfig(context.Background(), after); err != nil {
		t.Fatalf("reload: %v", err)
	}
	close(release)

	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not finish")
	}
	if r.err != nil {
		t.Fatalf("Route across the reload failed: %v", r.err)
	}
	if r.resp.Provider != "b" {
		t.Fatalf("served by %q, want b: a is no longer a target", r.resp.Provider)
	}
}

// An agentic MCP loop re-enters the walk on every turn with the strategy the
// request started with, and a reload can land between turns while a tool runs.
// Each turn's walk read the reloaded mode, so a loop whose first turn a served
// under fallback over [a, b] failed its second with 404 model_not_found once
// the config became single over [b], after the first turn had been billed.
func TestRoute_ReloadBetweenMCPTurnsKeepsTheLoopRouting(t *testing.T) {
	mcpSrv := newMCPTestServer(t)
	defer mcpSrv.Close()
	servers := []mcp.ServerConfig{{Name: "test-mcp", URL: mcpSrv.URL + "/mcp", TimeoutSeconds: 5}}
	before := config.Config{
		Strategy:   config.StrategyConfig{Mode: config.ModeFallback},
		Targets:    []config.Target{{VirtualKey: "a"}, {VirtualKey: "b"}},
		MCPServers: servers,
	}
	after := config.Config{
		Strategy:   config.StrategyConfig{Mode: config.ModeSingle},
		Targets:    []config.Target{{VirtualKey: "b"}},
		MCPServers: servers,
	}

	gw, err := newTestGateway(t, before)
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	var reloadErr atomic.Pointer[error]
	gw.RegisterProvider(&mockProvider{
		name:   "a",
		models: []string{"test-model"},
		completeFn: func(context.Context, providers.Request) (*providers.Response, error) {
			// The reload lands while the request is inside its loop: after this
			// turn has been walked, before the next one is.
			if err := gw.ReloadConfig(context.Background(), after); err != nil {
				reloadErr.Store(&err)
			}
			return mcpToolCallTurn("turn-1", "tc-1", providers.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}), nil
		},
	})
	gw.RegisterProvider(&mockProvider{
		name:   "b",
		models: []string{"test-model"},
		resp: &providers.Response{
			ID: "turn-2", Model: "test-model",
			Choices: []providers.Choice{{Message: providers.Message{Role: "assistant", Content: "42"}, FinishReason: "stop"}},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	select {
	case <-gw.MCPInitDone():
	case <-ctx.Done():
		t.Fatal("timed out waiting for MCP initialization")
	}
	if gw.mcpRegistry == nil || len(gw.mcpRegistry.AllTools()) == 0 {
		t.Fatal("no MCP tools registered; the loop would never run and this would pass vacuously")
	}

	resp, err := gw.Route(ctx, providers.Request{
		Model:    "test-model",
		Messages: []providers.Message{{Role: "user", Content: "What is the answer?"}},
	})
	if p := reloadErr.Load(); p != nil {
		t.Fatalf("reload: %v", *p)
	}
	if err != nil {
		t.Fatalf("Route failed on the turn after the reload: %v", err)
	}
	if resp.Provider != "b" {
		t.Fatalf("final turn served by %q, want b", resp.Provider)
	}
}

// The plan a strategy is walked under comes from the config that strategy was
// built from, not from whatever config is installed when the walk begins.
func TestPlanFor_ReadsTheModeTheStrategyWasBuiltUnder(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeFallback, FailoverOnStatusCodes: []int{409}},
		Targets:  []config.Target{{VirtualKey: "a"}, {VirtualKey: "b"}},
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	gw.RegisterProvider(reloadPlanProvider("a"))
	gw.RegisterProvider(reloadPlanProvider("b"))

	s, err := gw.getStrategy()
	if err != nil {
		t.Fatalf("getStrategy: %v", err)
	}
	if err := gw.ReloadConfig(context.Background(), config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "b"}},
	}); err != nil {
		t.Fatalf("reload: %v", err)
	}

	keys, err := s.SelectTargets(providers.Request{Model: "m"})
	if err != nil {
		t.Fatalf("SelectTargets: %v", err)
	}
	plan := gw.planFor(s, "m", keys)
	if !plan.advance {
		t.Fatal("plan for a fallback strategy does not advance after a reload to single")
	}
	if len(plan.failoverOn) != 1 || plan.failoverOn[0] != 409 {
		t.Fatalf("failover statuses = %v, want the fallback strategy's [409]", plan.failoverOn)
	}
}
