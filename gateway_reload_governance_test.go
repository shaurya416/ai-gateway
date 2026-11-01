package aigateway

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

// governedProvider streams and embeds, and records every prompt it is sent.
type governedProvider struct {
	mockProvider
	mu      sync.Mutex
	prompts []string
}

func (p *governedProvider) saw(text string) {
	p.mu.Lock()
	p.prompts = append(p.prompts, text)
	p.mu.Unlock()
}

func (p *governedProvider) received() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prompts...)
}

func (p *governedProvider) CompleteStream(_ context.Context, req providers.Request) (<-chan providers.StreamChunk, error) {
	for _, m := range req.Messages {
		p.saw(m.Content)
	}
	ch := make(chan providers.StreamChunk, 1)
	ch <- providers.StreamChunk{Choices: []providers.StreamChoice{{Delta: providers.MessageDelta{Content: "ok"}, FinishReason: "stop"}}}
	close(ch)
	return ch, nil
}

func (p *governedProvider) Embed(_ context.Context, req providers.EmbeddingRequest) (*providers.EmbeddingResponse, error) {
	if s, ok := req.Input.(string); ok {
		p.saw(s)
	}
	return &providers.EmbeddingResponse{Model: req.Model}, nil
}

// reloadGovernanceConfigs is a config routing everything to a with no
// guardrail, and the config that replaces it: everything to b, behind a word
// filter that keeps "secret" from ever reaching b.
func reloadGovernanceConfigs() (before, after config.Config) {
	before = config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "a"}},
	}
	after = config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "b"}},
		Plugins: []config.PluginConfig{{
			Name: "word-filter", Type: "guardrail", Stage: "before_request", Enabled: true,
			Config: map[string]any{"blocked_words": []any{"secret"}},
		}},
	}
	return before, after
}

// holdBeforeStage registers a before_request plugin on gw's current plugins
// that blocks until release is closed, closing entered as it starts.
func holdBeforeStage(t *testing.T, gw *Gateway) (entered, release chan struct{}) {
	t.Helper()
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	if err := gw.RegisterPlugin(plugin.StageBeforeRequest, &testPlugin{
		name: "hold",
		typ:  plugin.TypeLogging,
		execFn: func(ctx context.Context, _ *plugin.Context) error {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		},
	}); err != nil {
		t.Fatalf("register plugin: %v", err)
	}
	return entered, release
}

// reloadDuringBeforeStage runs request while a reload from before to after
// lands in the middle of its before_request stage, and returns the providers
// a and b it could have reached.
func reloadDuringBeforeStage(t *testing.T, request func(*Gateway) error) (a, b *governedProvider, err error) {
	t.Helper()
	before, after := reloadGovernanceConfigs()
	gw, newErr := newTestGateway(t, before)
	if newErr != nil {
		t.Fatalf("new gateway: %v", newErr)
	}
	a = &governedProvider{mockProvider: mockProvider{name: "a", models: []string{"m"}}}
	b = &governedProvider{mockProvider: mockProvider{name: "b", models: []string{"m"}}}
	gw.RegisterProvider(a)
	gw.RegisterProvider(b)
	entered, release := holdBeforeStage(t, gw)

	done := make(chan error, 1)
	go func() { done <- request(gw) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the before_request stage")
	}
	if reloadErr := gw.ReloadConfig(context.Background(), after); reloadErr != nil {
		t.Fatalf("reload: %v", reloadErr)
	}
	close(release)
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not finish")
	}
	return a, b, err
}

func requireNeverReached(t *testing.T, p *governedProvider) {
	t.Helper()
	for _, prompt := range p.received() {
		if strings.Contains(prompt, "secret") {
			t.Fatalf("%s was sent %q: the request ran the plugins of the config it started under and was routed by the targets of the one that replaced it, so it reached a target behind a guardrail that never saw it", p.name, prompt)
		}
	}
}

// A streamed request is governed by the plugins of one config and routed by
// that config's strategy. The plugins were read when the request started and
// the strategy only after the before_request stage, so a reload landing during
// the stage sent the request to the new config's target without the new
// config's guardrail having read it: a prompt the new word filter blocks
// reached the provider that filter exists to protect.
func TestRouteStream_ReloadDuringBeforeStageKeepsOneConfig(t *testing.T) {
	_, b, _ := reloadDuringBeforeStage(t, func(gw *Gateway) error {
		ch, err := gw.RouteStream(context.Background(), providers.Request{
			Model:    "m",
			Stream:   true,
			Messages: []providers.Message{{Role: "user", Content: "the secret is out"}},
		})
		if err != nil {
			return err
		}
		for range ch { //nolint:revive // drain the stream
		}
		return nil
	})
	requireNeverReached(t, b)
}

// The same holds on the surfaces that run the shared governance stage.
func TestEmbed_ReloadDuringBeforeStageKeepsOneConfig(t *testing.T) {
	_, b, _ := reloadDuringBeforeStage(t, func(gw *Gateway) error {
		_, err := gw.Embed(context.Background(), providers.EmbeddingRequest{Model: "m", Input: "the secret is out"})
		return err
	})
	requireNeverReached(t, b)
}
