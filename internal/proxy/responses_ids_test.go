package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/providers"
)

// nativeWireStub is a proxiable provider that does not speak the OpenAI wire —
// the shape anthropic, gemini and azure-openai have.
type nativeWireStub struct{ proxiableStub }

func (*nativeWireStub) NonOpenAIWire() {}

var _ providers.NonOpenAIWireProvider = (*nativeWireStub)(nil)

// A responses_target naming a non-OpenAI-wire provider is refused 501 on the id
// sub-routes, as it is on create. The id route checked only that the provider
// could be proxied, so it forwarded the request under that provider's
// credential to a path its upstream does not serve and relayed the answer.
func TestResponsesIDs_RefusesNonOpenAIWireTarget(t *testing.T) {
	up := newCountingUpstream(t)
	t.Setenv("FERRO_MODEL_CATALOG_TIMEOUT", "0")
	gw, err := aigateway.New(config.Config{
		Strategy:        config.StrategyConfig{Mode: config.ModeSingle},
		Targets:         []config.Target{{VirtualKey: "native", Models: []string{"stub-model"}}},
		ResponsesTarget: "native",
	})
	if err != nil {
		t.Fatalf("aigateway.New: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })
	gw.RegisterProvider(&nativeWireStub{proxiableStub{name: "native", baseURL: up.URL, models: []string{"stub-model"}}})

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/responses/resp_123", nil)
	w := httptest.NewRecorder()
	ResponsesIDs(gw)(w, r)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501; body: %s", w.Code, w.Body.String())
	}
	if hits := up.hits.Load(); hits != 0 {
		t.Errorf("upstream received %d requests, want 0 — the provider's credential was sent to a path it does not serve", hits)
	}
}
