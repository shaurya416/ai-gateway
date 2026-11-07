package aigateway

import (
	"errors"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// A model id a target serves can also be the name of a global alias: a catalog
// gpt-4o beside an alias sending gpt-4o to a model on a target whose credential
// has not rolled out. Every routed surface resolves the alias
// before anything else, so a request for that id is routed as the alias's
// target. /v1/models asked whether the id itself was routable instead, and
// listed one that every request for it was refused: advertised must stay a
// subset of routable, and the listing's routability question is the walk's.
func TestAdvertisedModelsAreRoutable_AliasShadowedID(t *testing.T) {
	const shadowed, aliasTarget = "gpt-4o", "gpt-4o-mini"

	listed := func(gw *Gateway, id string) bool {
		for _, m := range gw.AllModels() {
			if m.ID == id {
				return true
			}
		}
		return false
	}
	newGateway := func(t *testing.T, targets []config.Target, stubs ...*mockProvider) *Gateway {
		t.Helper()
		gw, err := newTestGateway(t, config.Config{
			Strategy: config.StrategyConfig{Mode: config.ModeFallback},
			Targets:  targets,
			Aliases:  map[string]string{shadowed: aliasTarget},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		for _, stub := range stubs {
			gw.RegisterProvider(stub)
		}
		return gw
	}
	request := providers.Request{Model: shadowed, Messages: []providers.Message{{Role: "user", Content: "ping"}}}

	t.Run("alias target served by no target", func(t *testing.T) {
		gw := newGateway(t, []config.Target{{VirtualKey: "azure-like"}},
			&mockProvider{name: "azure-like", models: []string{shadowed}, resp: &providers.Response{ID: "r"}})

		if _, err := gw.Route(t.Context(), request); !errors.Is(err, core.ErrNoCapableProvider) {
			t.Fatalf("precondition: Route(%q) err = %v, want no capable provider: the alias sends it to %q, which no target serves", shadowed, err, aliasTarget)
		}
		if listed(gw, shadowed) {
			t.Errorf("/v1/models lists %q, but every request for it is routed as %q and refused", shadowed, aliasTarget)
		}
	})

	t.Run("alias target served by another target", func(t *testing.T) {
		gw := newGateway(t, []config.Target{{VirtualKey: "azure-like"}, {VirtualKey: "remote"}},
			&mockProvider{name: "azure-like", models: []string{shadowed}, resp: &providers.Response{ID: "r"}},
			&mockProvider{name: "remote", models: []string{aliasTarget}, resp: &providers.Response{ID: "r"}})

		resp, err := gw.Route(t.Context(), request)
		if err != nil {
			t.Fatalf("precondition: Route(%q) err = %v, want it served as %q", shadowed, err, aliasTarget)
		}
		if resp.Provider != "remote" {
			t.Fatalf("precondition: Route(%q) served by %q, want remote", shadowed, resp.Provider)
		}
		if !listed(gw, shadowed) {
			t.Errorf("/v1/models omits %q, though a request for it is served", shadowed)
		}
	})
}
