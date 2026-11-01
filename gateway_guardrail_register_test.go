package aigateway

import (
	"context"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/observability"
	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

// A guardrail registered through the Go API reports its matches exactly as one
// loaded from config does. RegisterPlugin adds to the manager New builds, so a
// warn-mode guardrail registered there that matched every request was served
// with no gateway.guardrail.match event at all — the one signal a non-blocking
// mode exists to produce.
func TestGateway_RegisterPlugin_GuardrailMatchReachesTheSeam(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "mock"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ep := &matchCapturingProvider{eventCapturingProvider: eventCapturingProvider{recordingActive: true}}
	gw.SetObservability(ep)
	gw.RegisterProvider(&mockProvider{
		name:   "mock",
		models: []string{testModel},
		resp:   &providers.Response{ID: "r1", Provider: "mock", Model: testModel},
	})
	if err := gw.RegisterPlugin(plugin.StageBeforeRequest, &testPlugin{
		name: "audit-guard",
		typ:  plugin.TypeGuardrail,
		execFn: func(_ context.Context, pctx *plugin.Context) error {
			pctx.NoteGuardrailMatch(plugin.ActionWarn)
			return nil
		},
	}); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}

	if _, err := gw.Route(context.Background(), providers.Request{
		Model:    testModel,
		Messages: []providers.Message{{Role: "user", Content: "hello"}},
	}); err != nil {
		t.Fatalf("a warn action must not block the request, got %v", err)
	}

	matches := eventsWithSubject(ep.capturedEvents(), observability.SubjectGuardrailMatch)
	if len(matches) != 1 {
		t.Fatalf("want exactly one %s event, got %d", observability.SubjectGuardrailMatch, len(matches))
	}
	want := map[string]any{
		observability.AttrFerroGuardrailPlugin:  "audit-guard",
		observability.AttrFerroGuardrailAction:  plugin.ActionWarn,
		observability.AttrFerroGuardrailStage:   string(plugin.StageBeforeRequest),
		observability.AttrFerroGuardrailAllowed: true,
	}
	for k, v := range want {
		if got := matches[0].Attributes[k]; got != v {
			t.Errorf("attribute %q = %v, want %v", k, got, v)
		}
	}
}
