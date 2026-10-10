package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// gatedSource is a ProviderSource that also reports target membership, the way
// *Gateway does. Only the providers in targeted may receive a request body.
type gatedSource struct {
	*providers.Registry
	targeted map[string]bool
}

func (g gatedSource) IsTargetedProvider(name string) bool { return g.targeted[name] }

// ungatedSource has no target knowledge, like a bare *providers.Registry.
type ungatedSource struct{ *providers.Registry }

type stubProvider struct {
	name   string
	models []string
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) SupportsModel(model string) bool {
	for _, m := range s.models {
		if m == model {
			return true
		}
	}
	return false
}

func (s *stubProvider) Complete(context.Context, core.Request) (*core.Response, error) {
	return nil, nil
}

func newRegistry(t *testing.T, provs ...*stubProvider) *providers.Registry {
	t.Helper()
	reg := providers.NewRegistry()
	for _, p := range provs {
		reg.Register(p)
	}
	return reg
}

// F73: the pass-through reached any provider whose credential was in the
// environment, decided by model ownership or by a client-supplied X-Provider
// header. With targets: [groq], a request naming an openai model sent the
// prompt to openai under the gateway's own openai credential.
func TestResolveProvider_TargetGating(t *testing.T) {
	openai := &stubProvider{name: "openai", models: []string{"gpt-4o"}}
	groq := &stubProvider{name: "groq", models: []string{"llama-3.3-70b"}}

	src := gatedSource{
		Registry: newRegistry(t, openai, groq),
		targeted: map[string]bool{"groq": true}, // openai registered, NOT targeted
	}

	for _, tc := range []struct {
		name     string
		header   string
		body     string
		wantOK   bool
		wantName string
	}{
		{"model owned by an untargeted provider is refused", "", `{"model":"gpt-4o"}`, false, ""},
		{"model owned by a targeted provider resolves", "", `{"model":"llama-3.3-70b"}`, true, "groq"},
		{"header naming an untargeted provider is refused", "openai", `{}`, false, ""},
		{"header naming a targeted provider resolves", "groq", `{}`, true, "groq"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			if tc.header != "" {
				req.Header.Set("X-Provider", tc.header)
			}

			p, _, ok := ResolveProvider(req, src)
			if ok != tc.wantOK {
				t.Fatalf("resolved=%v, want %v", ok, tc.wantOK)
			}
			if ok && p.Name() != tc.wantName {
				t.Errorf("resolved to %q, want %q", p.Name(), tc.wantName)
			}
		})
	}
}

// The header is the documented escape hatch for endpoints whose model ids no
// index can enumerate — a fine-tune base model, a model newer than the catalog.
// Target gating must not close it: a model the index does not own still
// resolves, provided the provider is configured.
func TestResolveProvider_HeaderEscapeHatchSurvivesGating(t *testing.T) {
	groq := &stubProvider{name: "groq", models: []string{"llama-3.3-70b"}}
	src := gatedSource{
		Registry: newRegistry(t, groq),
		targeted: map[string]bool{"groq": true},
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/fine_tuning/jobs",
		strings.NewReader(`{"model":"ft:a-model-no-index-knows:org::abc123"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Provider", "groq")

	p, _, ok := ResolveProvider(req, src)
	if !ok {
		t.Fatal("header did not resolve an unenumerable model; the escape hatch is closed")
	}
	if p.Name() != "groq" {
		t.Errorf("resolved to %q, want groq", p.Name())
	}
}

// A source with no target knowledge has no allowlist to apply. Gating must be
// opt-in via the interface, not a default deny that would make a bare Registry
// resolve nothing at all.
func TestResolveProvider_UngatedSourceIsUnchanged(t *testing.T) {
	openai := &stubProvider{name: "openai", models: []string{"gpt-4o"}}
	src := ungatedSource{Registry: newRegistry(t, openai)}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Content-Type", "application/json")

	p, _, ok := ResolveProvider(req, src)
	if !ok {
		t.Fatal("a source without target knowledge must not gate")
	}
	if p.Name() != "openai" {
		t.Errorf("resolved to %q, want openai", p.Name())
	}
}

// configuredStub is a proxiable provider whose model set comes from its own
// config, so the routing index records it as an owner of those models.
type configuredStub struct{ proxiableStub }

func (p *configuredStub) ConfiguredModels() []string { return p.models }

// A model a configured target serves is forwarded to that target even when a
// provider no target names registered first and owns the same name. The
// pass-through placed the model among every registered provider and only then
// applied the targets allowlist, so it answered 404 model_not_found for a model
// the target declared, /v1/models advertised and chat routed.
func TestResolveProvider_TargetServesAModelAnUntargetedProviderAlsoOwns(t *testing.T) {
	t.Setenv("FERRO_MODEL_CATALOG_TIMEOUT", "0")
	untargeted := newCountingUpstream(t)
	targeted := newCountingUpstream(t)
	gw, err := aigateway.New(config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeFallback},
		Targets:  []config.Target{{VirtualKey: "second", Models: []string{"shared-model"}}},
	})
	if err != nil {
		t.Fatalf("aigateway.New: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })
	gw.RegisterProvider(&configuredStub{proxiableStub{name: "first", baseURL: untargeted.URL, models: []string{"shared-model", "first-only-model"}}})
	gw.RegisterProvider(&proxiableStub{name: "second", baseURL: targeted.URL})

	for name, h := range abortSurfaces(gw) {
		t.Run(name, func(t *testing.T) {
			before := targeted.hits.Load()
			w := postResponsesBody(t, h, `{"model":"shared-model","input":"hi"}`)
			if w.Code != http.StatusOK || w.Header().Get("X-Gateway-Provider") != "second" {
				t.Fatalf("shared model: got %d from %q, want 200 from the configured target; body: %s", w.Code, w.Header().Get("X-Gateway-Provider"), w.Body.String())
			}
			if got := targeted.hits.Load() - before; got != 1 {
				t.Errorf("configured target received %d requests, want 1", got)
			}

			// A model only the untargeted provider owns is still refused.
			if w := postResponsesBody(t, h, `{"model":"first-only-model","input":"hi"}`); w.Code != http.StatusNotFound {
				t.Errorf("untargeted provider's model: status = %d, want 404; body: %s", w.Code, w.Body.String())
			}
			if hits := untargeted.hits.Load(); hits != 0 {
				t.Errorf("provider no target names received %d requests, want 0", hits)
			}
		})
	}
}

var _ core.ProviderSource = gatedSource{}
