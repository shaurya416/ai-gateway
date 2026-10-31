package aigateway

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	_ "github.com/ferro-labs/ai-gateway/plugin/cache"
	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/test/redisstub"
)

// TestResponseCache_RedisSharedAcrossGateways runs two gateways — two
// replicas — against one Redis, configured the way an operator would: the URL
// carrying the password named by a ${VAR} reference. The request the first
// replica answered is served by the second from the shared cache, and the
// password never enters the Config either replica holds.
func TestResponseCache_RedisSharedAcrossGateways(t *testing.T) {
	srv := redisstub.New(t)
	srv.RequireAuth("", "replica-s3cret")
	t.Setenv("FERRO_TEST_REDIS_URL", "redis://:replica-s3cret@"+srv.Addr()+"/1")

	cacheCfg := map[string]any{"backend": "redis", "redis_url": "${FERRO_TEST_REDIS_URL}", "max_age": 60}
	replica := func() (*Gateway, *atomic.Int64) {
		t.Helper()
		gw, err := newTestGateway(t, config.Config{
			Strategy: config.StrategyConfig{Mode: config.ModeSingle},
			Targets:  []config.Target{{VirtualKey: "openai"}},
			Plugins: []config.PluginConfig{
				{Name: "response-cache", Type: "transform", Stage: "before_request", Enabled: true, Config: cacheCfg},
				{Name: "response-cache", Type: "transform", Stage: "after_request", Enabled: true, Config: cacheCfg},
			},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		var calls atomic.Int64
		gw.RegisterProviderAs("openai", &mockProvider{
			name:   "openai",
			models: []string{"gpt-4o"},
			completeFn: func(context.Context, providers.Request) (*providers.Response, error) {
				calls.Add(1)
				return &providers.Response{
					ID:      "chatcmpl-shared",
					Model:   "gpt-4o",
					Choices: []providers.Choice{{Message: providers.Message{Role: "assistant", Content: "from the provider"}, FinishReason: "stop"}},
				}, nil
			},
		})
		if err := gw.LoadPlugins(); err != nil {
			t.Fatalf("LoadPlugins: %v", err)
		}
		return gw, &calls
	}
	first, firstCalls := replica()
	second, secondCalls := replica()

	req := providers.Request{Model: "gpt-4o", Messages: []providers.Message{{Role: "user", Content: "hello"}}}
	if _, err := first.Route(context.Background(), req); err != nil {
		t.Fatalf("first replica: %v", err)
	}
	resp, err := second.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("second replica: %v", err)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("provider calls = %d and %d, want 1 and 0: the second replica should be served from Redis", firstCalls.Load(), secondCalls.Load())
	}
	if resp.ID != "chatcmpl-shared" || resp.Choices[0].Message.Content != "from the provider" {
		t.Fatalf("second replica served %#v", resp)
	}
	if keys := srv.Keys(1); len(keys) != 1 {
		t.Fatalf("database 1 holds %v, want the one shared entry", keys)
	}

	for _, gw := range []*Gateway{first, second} {
		got := gw.GetConfig().Plugins[0].Config["redis_url"]
		if got != "${FERRO_TEST_REDIS_URL}" || strings.Contains(got.(string), "replica-s3cret") {
			t.Fatalf("gateway Config redis_url = %v; it must still hold the reference", got)
		}
	}
}
