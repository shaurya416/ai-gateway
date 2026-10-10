package aigateway

import (
	"context"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
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

// TestRouteResponses_AfterPluginSeesThePrice is the request-logger contract for
// the one priced pass-through. The logger's cost column and any plugin reading
// Measurements see a Responses request's price there; the after_request stage
// was handed the duration alone, so every priced Responses row recorded its
// cost as unknown while the span and the completed event carried it. A generic
// pass-through stays unpriced, not free.
func TestRouteResponses_AfterPluginSeesThePrice(t *testing.T) {
	gw, _ := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "mock"}},
	})
	gw.catalog = aliasPricingCatalog()
	gw.RegisterProvider(&mockProvider{name: "mock", models: []string{testModel}})

	var measured []plugin.Measurements
	_ = gw.RegisterPlugin(plugin.StageAfterRequest, &testPlugin{
		name: "recorder",
		typ:  plugin.TypeLogging,
		execFn: func(_ context.Context, pctx *plugin.Context) error {
			measured = append(measured, pctx.Measurements)
			return nil
		},
	})

	usage := providers.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}
	if err := gw.RouteResponses(context.Background(), "mock", testModel, "hello", true, 0, &usage,
		func(context.Context) error { return nil }); err != nil {
		t.Fatalf("RouteResponses: %v", err)
	}
	if err := gw.RoutePassthrough(context.Background(), "mock", testModel, "hello", true,
		func(context.Context) error { return nil }); err != nil {
		t.Fatalf("RoutePassthrough: %v", err)
	}

	if len(measured) != 2 {
		t.Fatalf("after_request ran %d times, want 2", len(measured))
	}
	if responses := measured[0]; !responses.HasCost || responses.CostUSD != aliasPricingWantCostUSD {
		t.Errorf("responses Measurements = %+v, want HasCost with CostUSD %.5f", responses, aliasPricingWantCostUSD)
	}
	if passthrough := measured[1]; passthrough.HasCost || passthrough.CostUSD != 0 {
		t.Errorf("pass-through Measurements = %+v, want no cost: its response is opaque, so it is unpriced", passthrough)
	}
}
