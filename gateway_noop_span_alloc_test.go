package aigateway

import (
	"context"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/models"
	"github.com/ferro-labs/ai-gateway/observability"
	"github.com/ferro-labs/ai-gateway/pkg/metrics"
	"github.com/ferro-labs/ai-gateway/providers"
)

// With tracing off the request span is the NoOp provider's, which records
// nothing, so stamping it must cost nothing. SetAttribute takes an any, and
// boxing the provider and model strings into one allocated three times per
// request before the no-op discarded them — a fifth of a chat request's whole
// allocation count, spent on telemetry with tracing off. The comparison
// subtracts the metric-handle lookup, the one allocation these functions make
// for something other than the span.
func TestRequestOutcome_NoOpSpanStampsWithoutAllocating(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "mock"}},
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	gw.RegisterProvider(&mockProvider{name: "mock", models: []string{"gpt-4o"}})

	ctx := context.Background()
	obs := observability.NoOp()
	_, span := obs.StartRequestSpan(ctx, observability.RequestAttrs{})
	resp := &providers.Response{ID: "r1", Provider: "mock", Model: "gpt-4o", Usage: providers.Usage{PromptTokens: 5, CompletionTokens: 5}}
	record := surfaceRecord{provider: "mock", attempts: 1, routedModel: "gpt-4o"}
	cost := models.CostResult{}

	metricLookup := testing.AllocsPerRun(500, func() {
		_ = metrics.ForRequest("mock", gw.metricModel("gpt-4o"))
	})
	for name, stamp := range map[string]func(){
		"chat": func() {
			gw.recordOutcome(ctx, span, obs, resp, "gpt-4o", cost, time.Millisecond, resp.Provider, "", false, false, false, false)
		},
		"embeddings and images": func() {
			gw.recordSurfaceSuccess(ctx, span, obs, record, time.Millisecond, false, false)
		},
	} {
		stamp()
		if attributable := testing.AllocsPerRun(500, stamp) - metricLookup; attributable > 0 {
			t.Errorf("%s: stamping the NoOp request span allocated %v times per request, want 0", name, attributable)
		}
	}
}
