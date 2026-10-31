package aigateway

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/providers"
)

// countingEmbedder serves embeddings and counts its calls safely across
// goroutines.
type countingEmbedder struct {
	mockProvider
	calls atomic.Int64
}

func (c *countingEmbedder) Embed(context.Context, providers.EmbeddingRequest) (*providers.EmbeddingResponse, error) {
	c.calls.Add(1)
	return &providers.EmbeddingResponse{}, nil
}

// promptNotContainsConfig routes chat whose prompt lacks "unrelated-topic" to
// special. A promptless request matches no content rule, so on every non-chat
// surface it belongs to the first target that can serve it.
func promptNotContainsConfig(targets []config.Target) config.Config {
	return config.Config{
		Strategy: config.StrategyConfig{
			Mode: config.ModeContentBased,
			ContentConditions: []config.ContentCondition{
				{Type: config.ContentConditionPromptNotContains, Value: "unrelated-topic", TargetKey: "special"},
			},
		},
		Targets: targets,
	}
}

// The strategy a non-chat surface routes through under content-based is the
// one that answers a promptless request: the first target that can serve it.
// Built as the chat strategy instead, its prompt_not_contains rule matched every
// such request vacuously, and only a check made beside it — outside the lock
// the strategy is built under — kept it from being asked.
func TestStrategyFor_ContentBasedPromptlessSurfaceMatchesNoRule(t *testing.T) {
	targets := []config.Target{{VirtualKey: "first"}, {VirtualKey: "special"}}
	gw, err := newTestGateway(t, promptNotContainsConfig(targets))
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	gw.RegisterProvider(&countingEmbedder{mockProvider: mockProvider{name: "first", models: []string{"embed-model"}}})
	gw.RegisterProvider(&countingEmbedder{mockProvider: mockProvider{name: "special", models: []string{"embed-model"}}})

	s, err := gw.strategyFor(surfaceEmbeddings)
	if err != nil {
		t.Fatalf("strategyFor: %v", err)
	}
	keys, err := s.SelectTargets(providers.Request{Model: "embed-model"})
	if err != nil {
		t.Fatalf("SelectTargets: %v", err)
	}
	requireKeys(t, keys, "first")
}

// A reload that switches the routing mode to content-based must not let an
// embeddings request in flight be routed by a content rule. The mode used to
// be read under one hold of the lock and the strategy fetched under another,
// so a reload landing between the two handed the request a strategy whose
// prompt_not_contains rule matched it vacuously, and it went to that rule's
// target.
func TestEmbed_ReloadToContentBasedDoesNotMatchAContentRule(t *testing.T) {
	targets := []config.Target{{VirtualKey: "first"}, {VirtualKey: "special"}}
	fallback := config.Config{Strategy: config.StrategyConfig{Mode: config.ModeFallback}, Targets: targets}
	contentBased := promptNotContainsConfig(targets)

	gw, err := newTestGateway(t, fallback)
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	first := &countingEmbedder{mockProvider: mockProvider{name: "first", models: []string{"embed-model"}}}
	special := &countingEmbedder{mockProvider: mockProvider{name: "special", models: []string{"embed-model"}}}
	gw.RegisterProvider(first)
	gw.RegisterProvider(special)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_, _ = gw.Embed(context.Background(), providers.EmbeddingRequest{Model: "embed-model", Input: "hi"})
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			cfg := fallback
			if i%2 == 1 {
				cfg = contentBased
			}
			if err := gw.ReloadConfig(context.Background(), cfg); err != nil {
				t.Errorf("reload: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	if n := special.calls.Load(); n != 0 {
		t.Fatalf("special served %d of %d embeddings requests; under both configs every one belongs to first", n, n+first.calls.Load())
	}
	if first.calls.Load() == 0 {
		t.Fatal("no embeddings request was served")
	}
}
