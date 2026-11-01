package aigateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/pkg/logger"
	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/providers/groq"
)

// TestAllModels_DiscoveredModelIsOwnedByItsTarget pins a live-discovered
// model to the same owner every other /v1/models entry carries: the target
// that serves it.
//
// An OpenAI-compatible /models endpoint reports its own owned_by — Groq
// answers "Groq" (the shape providers/groq's discovery test reads), Mistral
// "mistralai", OpenAI "system" — and the listing published it unchanged. That
// named no target, so owned_by stopped describing where the request goes, and
// the catalog metadata /v1/models derives from owned_by + "/" + id was looked
// up under a vendor key the catalog does not use: with discovery on, every
// discovered model lost its context window and capabilities.
func TestAllModels_DiscoveredModelIsOwnedByItsTarget(t *testing.T) {
	const discoveredID = "llama-3.3-70b-versatile"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"` + discoveredID + `","object":"model","created":1700000000,"owned_by":"Groq"}]}`))
	}))
	t.Cleanup(upstream.Close)

	p, err := groq.New("test-key", upstream.URL+"/openai/v1")
	if err != nil {
		t.Fatalf("groq.New: %v", err)
	}
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeFallback},
		Targets:  []config.Target{{VirtualKey: "groq"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw.RegisterProvider(p)
	gw.runDiscovery(context.Background(), logger.Default())

	var found *providers.ModelInfo
	for _, m := range gw.AllModels() {
		if m.ID == discoveredID {
			found = &m
			break
		}
	}
	if found == nil {
		t.Fatalf("precondition: discovered model %q is not advertised", discoveredID)
	}
	if found.OwnedBy != "groq" {
		t.Errorf("%q owned_by = %q, want %q: the target that serves it", discoveredID, found.OwnedBy, "groq")
	}
	if found.Created != 1700000000 {
		t.Errorf("%q created = %d, want the discovered 1700000000", discoveredID, found.Created)
	}
	if _, ok := gw.Catalog().Get(found.OwnedBy + "/" + found.ID); !ok {
		t.Errorf("catalog has no entry %q, so /v1/models serves %q without its metadata",
			found.OwnedBy+"/"+found.ID, discoveredID)
	}
	if got := gw.ModelsFor("groq"); len(got) == 0 || got[0].OwnedBy != "groq" {
		t.Errorf("ModelsFor(groq) = %+v, want entries owned by groq", got)
	}
}
