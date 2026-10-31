package aigateway

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// A load-balance target weighted 0 is drained: it receives none of the load,
// which is how a target is taken out of rotation ahead of revoking its
// credential. That has to hold when its weighted sibling fails, too — the
// moment the sibling answers 5xx, opens its circuit, or is parked after a 429
// is not a moment to start sending traffic to a credential that is about to
// stop working.
func TestLoadBalance_DrainedTargetTakesNoFailoverTraffic(t *testing.T) {
	for _, tc := range []struct {
		name    string
		breaker *config.CircuitBreakerConfig
		fail    func() error
	}{
		{
			name: "sibling answers 5xx",
			fail: func() error { return core.StatusError("live", http.StatusServiceUnavailable, "overloaded") },
		},
		{
			name: "sibling is parked after a 429",
			fail: func() error {
				err := core.StatusError("live", http.StatusTooManyRequests, "slow down")
				err.RetryAfter = time.Minute
				return err
			},
		},
		{
			name:    "sibling circuit is open",
			breaker: &config.CircuitBreakerConfig{FailureThreshold: 1, SuccessThreshold: 1, MaxHalfThreshold: 1, Timeout: "1m"},
			fail:    func() error { return core.StatusError("live", http.StatusInternalServerError, "boom") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw, err := newTestGateway(t, config.Config{
				Strategy: config.StrategyConfig{Mode: config.ModeLoadBalance},
				Targets: []config.Target{
					{VirtualKey: "live", Weight: 1, CircuitBreaker: tc.breaker},
					{VirtualKey: "drained", Weight: 0},
				},
			})
			if err != nil {
				t.Fatalf("new gateway: %v", err)
			}
			gw.RegisterProvider(newCountingProvider("live", func() (*providers.Response, error) {
				return nil, tc.fail()
			}))
			drained := newCountingProvider("drained", func() (*providers.Response, error) {
				return &providers.Response{ID: "served-by-drained", Model: pipelineModel}, nil
			})
			gw.RegisterProvider(drained)

			for i := range 5 {
				resp, routeErr := gw.Route(context.Background(), pipelineRequest())
				if routeErr == nil {
					t.Fatalf("request %d served by %q, want the sibling's failure", i, resp.ID)
				}
			}
			if n := drained.calls.Load(); n != 0 {
				t.Errorf("drained target was called %d times; weight 0 must mean zero traffic", n)
			}
		})
	}
}
