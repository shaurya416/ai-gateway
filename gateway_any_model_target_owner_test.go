package aigateway

import (
	"context"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/providers"
)

// A provider declaring core.AnyModelProvider is offered every model no TARGET
// owns. A provider that is registered because its credential is in the
// environment, but that no target names, serves nothing — so it cannot own a
// model on a target's behalf either. Its catalog entry for gpt-4o-mini made the
// model "claimed", and the wildcard target — an Azure AI Foundry or Databricks
// deployment an operator named after the model — refused it with 404 for as
// long as an unrelated OPENAI_API_KEY was present.
func TestRouting_UntargetedOwnerDoesNotShadowAWildcardTarget(t *testing.T) {
	const model = "gpt-4o-mini"

	for _, mode := range []config.StrategyMode{config.ModeSingle, config.ModeFallback, config.ModeLoadBalance} {
		t.Run(string(mode), func(t *testing.T) {
			gw, err := newTestGateway(t, config.Config{
				Strategy: config.StrategyConfig{Mode: mode},
				Targets:  []config.Target{{VirtualKey: "databricks", Weight: 1}},
			})
			if err != nil {
				t.Fatalf("new gateway: %v", err)
			}
			wildcard := &declaredWildcardProvider{claimsEverythingProvider: *claimsEverything("databricks")}
			gw.RegisterProvider(wildcard)
			// Registered — its credential is present — and named by no target.
			untargeted := ownerOf("openai", model)
			gw.RegisterProvider(untargeted)

			resp, err := chat(gw, model)
			if err != nil {
				t.Fatalf("Route(%q): %v; no target owns the model, so the wildcard target must be offered it", model, err)
			}
			if resp.Provider != "databricks" {
				t.Errorf("served by %q, want databricks", resp.Provider)
			}
			if seen := untargeted.seen(); len(seen) != 0 {
				t.Errorf("untargeted provider received %v; it is named by no target", seen)
			}

			ch, err := gw.RouteStream(context.Background(), providers.Request{
				Model:    model,
				Stream:   true,
				Messages: []providers.Message{{Role: roleUser, Content: "canary"}},
			})
			if err != nil {
				t.Fatalf("RouteStream(%q): %v", model, err)
			}
			drainStream(t, ch)

			if p, ok := gw.FindTargetByModel(model); !ok || p.Name() != "databricks" {
				t.Errorf("FindTargetByModel(%q) = %v, %v; want databricks, true", model, p, ok)
			}
		})
	}
}

// The other half of the rule stands: a model a TARGET owns is never offered to
// a wildcard target, whichever order the targets are listed in.
func TestRouting_TargetedOwnerStillOutranksAWildcardTarget(t *testing.T) {
	const model = "gpt-4o-mini"

	gw := fallbackGateway(t, "databricks", "openai")
	wildcard := &declaredWildcardProvider{claimsEverythingProvider: *claimsEverything("databricks")}
	gw.RegisterProvider(wildcard)
	gw.RegisterProvider(ownerOf("openai", model))

	resp, err := chat(gw, model)
	if err != nil {
		t.Fatalf("Route(%q): %v", model, err)
	}
	if resp.Provider != "openai" {
		t.Errorf("served by %q, want openai", resp.Provider)
	}
	if seen := wildcard.seen(); len(seen) != 0 {
		t.Errorf("wildcard target received %v for a model a target owns", seen)
	}
}
