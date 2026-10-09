package aigateway

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	_ "github.com/ferro-labs/ai-gateway/plugin/cache"
	"github.com/ferro-labs/ai-gateway/providers"
)

// TestResponseCache_RoutingMetadataSeparatesEntries pins that two requests a
// conditional metadata rule routes to different targets never share a cache
// entry. Here the targets map the same routed model onto different upstream
// models; with metadata outside the cache key, the second tier was served the
// first tier's answer from a model it was never routed to.
func TestResponseCache_RoutingMetadataSeparatesEntries(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{
			Mode: config.ModeConditional,
			Conditions: []config.Condition{
				{Key: config.ConditionKeyMetadata, Field: "tier", Value: "gold", TargetKey: "gold"},
			},
		},
		Targets: []config.Target{
			{VirtualKey: "standard", ModelMap: map[string]string{"chat": "chat-mini"}},
			{VirtualKey: "gold", ModelMap: map[string]string{"chat": "chat-large"}},
		},
		Plugins: cacheEntries(),
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	var calls atomic.Int64
	answer := func(_ context.Context, req providers.Request) (*providers.Response, error) {
		calls.Add(1)
		return &providers.Response{
			ID:      "resp-" + req.Model,
			Model:   req.Model,
			Choices: []providers.Choice{{Message: providers.Message{Role: "assistant", Content: "answered by " + req.Model}}},
		}, nil
	}
	models := []string{"chat", "chat-mini", "chat-large"}
	gw.RegisterProviderAs("standard", &mockProvider{name: "standard", models: models, completeFn: answer})
	gw.RegisterProviderAs("gold", &mockProvider{name: "gold", models: models, completeFn: answer})
	if err := gw.LoadPlugins(); err != nil {
		t.Fatalf("load plugins: %v", err)
	}

	request := func(tier string) providers.Request {
		return providers.Request{
			Model:           "chat",
			Messages:        []providers.Message{{Role: "user", Content: "hello"}},
			RoutingMetadata: map[string]string{"tier": tier},
		}
	}

	first, err := gw.Route(context.Background(), request("standard"))
	if err != nil {
		t.Fatalf("standard request: %v", err)
	}
	if got := first.Choices[0].Message.Content; got != "answered by chat-mini" {
		t.Fatalf("standard tier answer = %q, want the standard target's model", got)
	}

	second, err := gw.Route(context.Background(), request("gold"))
	if err != nil {
		t.Fatalf("gold request: %v", err)
	}
	if got := second.Choices[0].Message.Content; got != "answered by chat-large" {
		t.Fatalf("gold tier answer = %q, want the gold target's model (served from the standard tier's cache entry)", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("provider calls = %d, want 2 (one per routed target)", got)
	}

	// The same tier asking the same question is still a cache hit.
	if _, err := gw.Route(context.Background(), request("gold")); err != nil {
		t.Fatalf("repeat gold request: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("provider calls after a repeat = %d, want 2 (the repeat is a cache hit)", got)
	}
}
