package aigateway

import (
	"context"
	"net/http"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// A zero-weight A/B variant receives no traffic at all — that is what makes it
// usable to drain a variant before its credential is revoked. ab-test is a
// pool mode, so the walk carries a failed request past the drawn variant; the
// drained variant must not be where it lands, whether the live variant
// answered 5xx or its circuit is open.
func TestABTest_DrainedVariantTakesNoFailoverTraffic(t *testing.T) {
	for _, tc := range []struct {
		name string
		live config.Target
		trip bool
	}{
		{name: "live variant fails", live: config.Target{VirtualKey: "live"}},
		{name: "live variant's circuit open", live: openableTarget("live"), trip: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw, err := newTestGateway(t, config.Config{
				Strategy: config.StrategyConfig{
					Mode: config.ModeABTest,
					ABVariants: []config.ABVariantConfig{
						{TargetKey: "drained", Weight: 0, Label: "old"},
						{TargetKey: "live", Weight: 100, Label: "new"},
					},
				},
				Targets: []config.Target{{VirtualKey: "drained"}, tc.live},
			})
			if err != nil {
				t.Fatalf("new gateway: %v", err)
			}
			drained := newCountingProvider("drained", func() (*providers.Response, error) {
				return &providers.Response{ID: "served-by-drained", Model: pipelineModel}, nil
			})
			live := newCountingProvider("live", func() (*providers.Response, error) {
				return nil, core.StatusError("live", http.StatusServiceUnavailable, "down")
			})
			gw.RegisterProvider(drained)
			gw.RegisterProvider(live)
			if tc.trip {
				tripBreaker(t, gw, "live")
			}

			resp, err := gw.Route(context.Background(), pipelineRequest())
			if err == nil {
				t.Fatalf("served by %q: a drained variant took the live variant's failover", resp.Provider)
			}
			if n := drained.calls.Load(); n != 0 {
				t.Fatalf("the drained variant was called %d times", n)
			}
		})
	}
}
