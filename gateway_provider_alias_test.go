package aigateway

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/models"
	"github.com/ferro-labs/ai-gateway/observability"
	"github.com/ferro-labs/ai-gateway/pkg/metrics"
	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

type aliasCapabilityProvider struct{}

var (
	_ providers.Provider                = (*aliasCapabilityProvider)(nil)
	_ providers.StreamProvider          = (*aliasCapabilityProvider)(nil)
	_ providers.EmbeddingProvider       = (*aliasCapabilityProvider)(nil)
	_ providers.ImageProvider           = (*aliasCapabilityProvider)(nil)
	_ providers.RerankProvider          = (*aliasCapabilityProvider)(nil)
	_ providers.ModerationProvider      = (*aliasCapabilityProvider)(nil)
	_ providers.TranscriptionProvider   = (*aliasCapabilityProvider)(nil)
	_ providers.SpeechProvider          = (*aliasCapabilityProvider)(nil)
	_ providers.ProxiableProvider       = (*aliasCapabilityProvider)(nil)
	_ providers.NonOpenAIWireProvider   = (*aliasCapabilityProvider)(nil)
	_ providers.RequestSigner           = (*aliasCapabilityProvider)(nil)
	_ providers.BatchProvider           = (*aliasCapabilityProvider)(nil)
	_ providers.DiscoveryProvider       = (*aliasCapabilityProvider)(nil)
	_ providers.ConfiguredModelProvider = (*aliasCapabilityProvider)(nil)
	_ providers.AnyModelProvider        = (*aliasCapabilityProvider)(nil)
)

func (*aliasCapabilityProvider) Name() string { return "canonical" }

func (*aliasCapabilityProvider) Complete(_ context.Context, req providers.Request) (*providers.Response, error) {
	return &providers.Response{Model: req.Model}, nil
}

func (*aliasCapabilityProvider) SupportsModel(string) bool { return true }
func (*aliasCapabilityProvider) ServesAnyModel()           {}

func (*aliasCapabilityProvider) ConfiguredModels() []string { return []string{"configured-model"} }

func (*aliasCapabilityProvider) CompleteStream(context.Context, providers.Request) (<-chan providers.StreamChunk, error) {
	ch := make(chan providers.StreamChunk)
	close(ch)
	return ch, nil
}

func (*aliasCapabilityProvider) Embed(_ context.Context, req providers.EmbeddingRequest) (*providers.EmbeddingResponse, error) {
	return &providers.EmbeddingResponse{Model: req.Model}, nil
}

func (*aliasCapabilityProvider) GenerateImage(context.Context, providers.ImageRequest) (*providers.ImageResponse, error) {
	return &providers.ImageResponse{}, nil
}

func (*aliasCapabilityProvider) Rerank(_ context.Context, req providers.RerankRequest) (*providers.RerankResponse, error) {
	return &providers.RerankResponse{Model: req.Model}, nil
}

func (*aliasCapabilityProvider) Moderate(_ context.Context, req providers.ModerationRequest) (*providers.ModerationResponse, error) {
	return &providers.ModerationResponse{Model: req.Model}, nil
}

func (*aliasCapabilityProvider) Transcribe(context.Context, providers.TranscriptionRequest) (*providers.TranscriptionResponse, error) {
	return &providers.TranscriptionResponse{Text: "ok"}, nil
}

func (*aliasCapabilityProvider) Speech(context.Context, providers.SpeechRequest) (*providers.SpeechResponse, error) {
	return &providers.SpeechResponse{Audio: []byte("ok"), ContentType: "audio/mpeg"}, nil
}

func (*aliasCapabilityProvider) BaseURL() string                      { return "https://example.com/v1" }
func (*aliasCapabilityProvider) AuthHeaders() map[string]string       { return nil }
func (*aliasCapabilityProvider) NonOpenAIWire()                       {}
func (*aliasCapabilityProvider) SignProxyRequest(*http.Request) error { return nil }
func (*aliasCapabilityProvider) BatchBaseURL() string                 { return "https://example.com/v1" }
func (*aliasCapabilityProvider) BatchAuthHeaders() map[string]string  { return nil }

func (*aliasCapabilityProvider) DiscoverModels(context.Context) ([]providers.ModelInfo, error) {
	return []providers.ModelInfo{{ID: "discovered-model"}}, nil
}

func TestRegisterProviderAsPreservesEveryOptionalCapability(t *testing.T) {
	const alias = "canonical::credential-1"
	provider := &aliasCapabilityProvider{}
	gw, err := New(config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: alias, Models: []string{"alias-model"}}},
	})
	if err != nil {
		t.Fatalf("create gateway: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })
	gw.RegisterProvider(provider)
	gw.RegisterProviderAs(alias, provider)

	if _, ok := gw.GetProvider("canonical"); !ok {
		t.Fatal("canonical provider was not registered")
	}
	registered, ok := gw.GetProvider(alias)
	if !ok {
		t.Fatalf("provider %q was not registered", alias)
	}
	if registered.Name() != alias {
		t.Fatalf("registered name = %q, want %q", registered.Name(), alias)
	}
	if got := gw.ListProviders(); !slices.Equal(got, []string{"canonical", alias}) {
		t.Fatalf("registered providers = %v, want [canonical %s]", got, alias)
	}

	assertCapability := func(name string, ok bool) {
		t.Helper()
		if !ok {
			t.Errorf("alias lost %s", name)
		}
	}
	_, ok = providers.As[providers.StreamProvider](registered)
	assertCapability("StreamProvider", ok)
	_, ok = providers.As[providers.EmbeddingProvider](registered)
	assertCapability("EmbeddingProvider", ok)
	_, ok = providers.As[providers.ImageProvider](registered)
	assertCapability("ImageProvider", ok)
	_, ok = providers.As[providers.RerankProvider](registered)
	assertCapability("RerankProvider", ok)
	_, ok = providers.As[providers.ModerationProvider](registered)
	assertCapability("ModerationProvider", ok)
	_, ok = providers.As[providers.TranscriptionProvider](registered)
	assertCapability("TranscriptionProvider", ok)
	_, ok = providers.As[providers.SpeechProvider](registered)
	assertCapability("SpeechProvider", ok)
	_, ok = providers.As[providers.ProxiableProvider](registered)
	assertCapability("ProxiableProvider", ok)
	_, ok = providers.As[providers.NonOpenAIWireProvider](registered)
	assertCapability("NonOpenAIWireProvider", ok)
	_, ok = providers.As[providers.RequestSigner](registered)
	assertCapability("RequestSigner", ok)
	_, ok = providers.As[providers.BatchProvider](registered)
	assertCapability("BatchProvider", ok)
	_, ok = providers.As[providers.DiscoveryProvider](registered)
	assertCapability("DiscoveryProvider", ok)
	_, ok = providers.As[providers.ConfiguredModelProvider](registered)
	assertCapability("ConfiguredModelProvider", ok)
	_, ok = providers.As[providers.AnyModelProvider](registered)
	assertCapability("AnyModelProvider", ok)

	streamProvider, ok := gw.FindStreamingByModel("alias-model")
	if !ok {
		t.Fatal("streaming alias was not indexed")
	}
	if _, err := streamProvider.CompleteStream(context.Background(), providers.Request{Model: "alias-model"}); err != nil {
		t.Fatalf("stream through alias: %v", err)
	}

	ctx := context.Background()
	if _, err := gw.Embed(ctx, providers.EmbeddingRequest{Model: "alias-model", Input: "hello"}); err != nil {
		t.Fatalf("embed through alias: %v", err)
	}
	if _, err := gw.GenerateImage(ctx, providers.ImageRequest{Model: "alias-model", Prompt: "hello"}); err != nil {
		t.Fatalf("image through alias: %v", err)
	}
	if _, err := gw.Rerank(ctx, providers.RerankRequest{Model: "alias-model", Query: "q", Documents: []string{"d"}}); err != nil {
		t.Fatalf("rerank through alias: %v", err)
	}
	if _, err := gw.Moderate(ctx, providers.ModerationRequest{Model: "alias-model", Input: "hello"}); err != nil {
		t.Fatalf("moderation through alias: %v", err)
	}
	if _, err := gw.Transcribe(ctx, providers.TranscriptionRequest{Model: "alias-model", File: []byte("audio"), Filename: "test.wav"}); err != nil {
		t.Fatalf("transcription through alias: %v", err)
	}
	if _, err := gw.Speech(ctx, providers.SpeechRequest{Model: "alias-model", Input: "hello", Voice: "test"}); err != nil {
		t.Fatalf("speech through alias: %v", err)
	}
}

func sortedModelIDs(infos []providers.ModelInfo) []string {
	ids := make([]string, len(infos))
	for i, m := range infos {
		ids[i] = m.ID
	}
	slices.Sort(ids)
	return ids
}

// TestRegisterProviderAsPreservesCatalogIdentity guards the routing-key /
// canonical-identity split: a catalog-backed provider registered under a
// distinct alias must still inherit its canonical provider's catalog models,
// without the caller having to re-declare them on targets[].models.
func TestRegisterProviderAsPreservesCatalogIdentity(t *testing.T) {
	t.Setenv(models.CatalogURLEnv, "file:///ferro-tests-use-embedded-catalog")
	entry, ok := providers.GetProviderEntry(providers.NameOpenAI)
	if !ok {
		t.Fatal("openai provider entry missing from registry")
	}
	provider, err := entry.Build(providers.ProviderConfig{providers.CfgKeyAPIKey: "test-key"})
	if err != nil {
		t.Fatalf("build openai provider: %v", err)
	}
	gw, err := New(config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "openai::credential-1"}}, // no Models declared
	})
	if err != nil {
		t.Fatalf("create gateway: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })

	gw.RegisterProvider(provider)                           // canonical "openai"
	gw.RegisterProviderAs("openai::credential-1", provider) // alias of the same provider

	canonical := sortedModelIDs(gw.ModelsFor("openai"))
	if len(canonical) == 0 {
		t.Skip("embedded catalog carries no openai models; cannot verify alias identity")
	}
	alias := sortedModelIDs(gw.ModelsFor("openai::credential-1"))
	if !slices.Equal(alias, canonical) {
		t.Fatalf("alias catalog models (%d) != canonical (%d); alias lost catalog identity",
			len(alias), len(canonical))
	}
}

// TestRegisterProviderAs_SuccessIsAttributedToTheTarget covers a provider that
// stamps its own name on the responses it returns, as every in-tree provider
// does, registered under a routing alias — the multi-credential binding
// RegisterProviderAs exists for. A failure was attributed to the target and a
// success to the provider's own name, so every per-target series — the request
// counter, the span's target key, the request-log row — showed that target
// failing every request it took and serving none.
func TestRegisterProviderAs_SuccessIsAttributedToTheTarget(t *testing.T) {
	const alias = "mock::credential-attribution"
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: alias}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fp := &fakeProvider{}
	gw.SetObservability(fp)

	var logged, target string
	if err := gw.RegisterPlugin(plugin.StageAfterRequest, &testPlugin{
		name: "attribution-probe",
		typ:  plugin.TypeLogging,
		execFn: func(_ context.Context, pctx *plugin.Context) error {
			logged, target = pctx.Response.Provider, pctx.Target
			return nil
		},
	}); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	gw.RegisterProviderAs(alias, &mockProvider{
		name:   "mock",
		models: []string{testModel},
		completeFn: func(context.Context, providers.Request) (*providers.Response, error) {
			return &providers.Response{ID: "r1", Provider: "mock", Model: testModel}, nil
		},
	})

	successes := metrics.RequestsTotal.WithLabelValues(alias, testModel, "success")
	before := counterValue(t, successes)
	resp, err := gw.Route(context.Background(), providers.Request{
		Model:    testModel,
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	if resp.Provider != alias {
		t.Errorf("resp.Provider = %q, want the target %q", resp.Provider, alias)
	}
	if logged != alias || target != alias {
		t.Errorf("after_request saw Response.Provider %q and Target %q, want both %q", logged, target, alias)
	}
	if got := counterValue(t, successes) - before; got != 1 {
		t.Errorf("successes counted under the target = %v, want 1", got)
	}
	if got := fp.rootSpan().attrs[observability.AttrFerroRoutingTargetKey]; got != alias {
		t.Errorf("span target key = %v, want %q", got, alias)
	}
}
