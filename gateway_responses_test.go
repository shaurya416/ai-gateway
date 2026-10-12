package aigateway

import (
	"context"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/models"
	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

// completedEvent returns the single gateway.request.completed event captured, or
// fails.
func completedEvent(t *testing.T, ep *eventCapturingProvider) (tokensIn, tokensOut int) {
	t.Helper()
	for _, ev := range ep.capturedEvents() {
		if strings.HasSuffix(ev.Subject, ".completed") {
			return ev.TokensIn, ev.TokensOut
		}
	}
	t.Fatal("no gateway.request.completed event was recorded")
	return 0, 0
}

// TestRouteResponses_PricesFromCapturedUsage proves the one thing that separates
// Responses from an ordinary pass-through: usage the forward teed off the
// response is priced into the record, so the completed event carries token
// counts instead of the unpriced blank a generic pass-through records.
func TestRouteResponses_PricesFromCapturedUsage(t *testing.T) {
	gw, _ := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "mock"}},
	})
	ep := &eventCapturingProvider{recordingActive: true}
	gw.SetObservability(ep)
	gw.RegisterProvider(&mockProvider{name: "mock", models: []string{testModel}})

	var usage providers.Usage
	err := gw.RouteResponses(context.Background(), "mock", testModel, "hello", true, 0, &usage,
		func(context.Context) error {
			// The tee fills usage during the forward, before RouteResponses reads it.
			usage = providers.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}
			return nil
		})
	if err != nil {
		t.Fatalf("RouteResponses: %v", err)
	}

	in, out := completedEvent(t, ep)
	if in != 100 || out != 50 {
		t.Errorf("completed event tokens = %d in / %d out, want 100/50 (usage teed off the response must be priced)", in, out)
	}
}

// responsesAfterStageMeasurements runs one Responses forward whose tee captures
// captured, against a catalog pricing the model at $2/M input and $8/M output,
// and returns the Measurements the after_request stage was handed — the ones
// the request logger persists cost_usd from.
func responsesAfterStageMeasurements(t *testing.T, captured providers.Usage) plugin.Measurements {
	t.Helper()
	gw, _ := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "mock"}},
	})
	gw.RegisterProvider(&mockProvider{name: "mock", models: []string{testModel}})

	inputPerM, outputPerM := 2.0, 8.0
	gw.mu.Lock()
	gw.catalog = models.Catalog{
		"mock/" + testModel: {
			Provider: "mock",
			ModelID:  testModel,
			Mode:     models.ModeChat,
			Pricing:  models.Pricing{InputPerMTokens: &inputPerM, OutputPerMTokens: &outputPerM},
		},
	}
	gw.mu.Unlock()

	var measured plugin.Measurements
	var ran bool
	if err := gw.RegisterPlugin(plugin.StageAfterRequest, &testPlugin{
		name: "recorder",
		typ:  plugin.TypeLogging,
		execFn: func(_ context.Context, pctx *plugin.Context) error {
			ran = true
			measured = pctx.Measurements
			return nil
		},
	}); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}

	var usage providers.Usage
	err := gw.RouteResponses(context.Background(), "mock", testModel, "hello", true, 0, &usage,
		func(context.Context) error {
			usage = captured
			return nil
		})
	if err != nil {
		t.Fatalf("RouteResponses: %v", err)
	}
	if !ran {
		t.Fatal("after_request stage did not run")
	}
	return measured
}

// TestRouteResponses_PricedCostReachesAfterRequestPlugins pins the cost the
// after_request stage is handed. The request logger persists cost_usd from
// these Measurements, so a priced Responses call that handed the stage no cost
// was written to the request log as "cost unknown" — even though the span, the
// metric and the completed event all carried the price.
func TestRouteResponses_PricedCostReachesAfterRequestPlugins(t *testing.T) {
	measured := responsesAfterStageMeasurements(t,
		providers.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000, TotalTokens: 2_000_000})

	if !measured.HasCost {
		t.Error("HasCost = false: a Responses call the catalog prices reached the after_request stage as unpriced")
	}
	if measured.CostUSD != 10.0 {
		t.Errorf("CostUSD = %v, want 10.0 (1M input at $2/M + 1M output at $8/M)", measured.CostUSD)
	}
}

// TestRouteResponses_NoUsage_ReachesAfterRequestPluginsUnpriced is the other
// direction: a forward that captured no usage hands the stage no cost, so its
// row reads "cost unknown" rather than a $0.00 bill.
func TestRouteResponses_NoUsage_ReachesAfterRequestPluginsUnpriced(t *testing.T) {
	measured := responsesAfterStageMeasurements(t, providers.Usage{})

	if measured.HasCost {
		t.Error("HasCost = true with no usage captured: a $0.00 bill recorded as though it were the real figure")
	}
	if measured.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want 0: nothing was captured to price", measured.CostUSD)
	}
}

// TestRouteResponses_NoUsage_StaysUnpriced confirms the generic pass-through
// behaviour is preserved when the forward captured nothing (an error, or an
// upstream that returned no usage): the record is unpriced, not billed as zero.
func TestRouteResponses_NoUsage_StaysUnpriced(t *testing.T) {
	gw, _ := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "mock"}},
	})
	ep := &eventCapturingProvider{recordingActive: true}
	gw.SetObservability(ep)
	gw.RegisterProvider(&mockProvider{name: "mock", models: []string{testModel}})

	var usage providers.Usage // left zero by the forward
	err := gw.RouteResponses(context.Background(), "mock", testModel, "hello", true, 0, &usage,
		func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("RouteResponses: %v", err)
	}

	in, out := completedEvent(t, ep)
	if in != 0 || out != 0 {
		t.Errorf("completed event tokens = %d/%d, want 0/0 when no usage was captured", in, out)
	}
}
