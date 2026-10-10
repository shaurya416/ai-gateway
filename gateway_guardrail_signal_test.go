package aigateway

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/observability"
	"github.com/ferro-labs/ai-gateway/providers"

	_ "github.com/ferro-labs/ai-gateway/plugin/regexguard"
	_ "github.com/ferro-labs/ai-gateway/plugin/secretscan"
)

// TestGateway_Route_EmitsGuardrailMatchEvent proves the log-only visibility the
// feature exists for: a regex-guard configured action: log matches the request,
// the request is served, and exactly one gateway.guardrail.match event reaches
// the observability seam — attributed to the instance, carrying the resolved
// action, and carrying no matched text.
func TestGateway_Route_EmitsGuardrailMatchEvent(t *testing.T) {
	const canary = "canary-CONTENT-9f3" // the matched text; must never appear in the event

	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "mock"}},
		Plugins: []config.PluginConfig{{
			Name:    "regex-guard",
			ID:      "audit-hits",
			Type:    "guardrail",
			Stage:   "before_request",
			Enabled: true,
			Config: map[string]any{
				"action": "log",
				"rules": []any{
					map[string]any{"name": "r1", "pattern": canary, "apply_to": "input"},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ep := &matchCapturingProvider{eventCapturingProvider: eventCapturingProvider{recordingActive: true}}
	gw.SetObservability(ep)
	if err := gw.LoadPlugins(); err != nil {
		t.Fatalf("LoadPlugins: %v", err)
	}
	gw.RegisterProvider(&mockProvider{
		name:   "mock",
		models: []string{testModel},
		resp:   &providers.Response{ID: "r1", Provider: "mock", Model: testModel},
	})

	// The rule matches in BOTH messages; the request still yields one signal.
	_, err = gw.Route(context.Background(), providers.Request{
		Model: testModel,
		Messages: []providers.Message{
			{Role: "user", Content: "please handle " + canary + " now"},
			{Role: "user", Content: "and again " + canary},
		},
	})
	if err != nil {
		t.Fatalf("a log action must not block the request, got %v", err)
	}

	matches := eventsWithSubject(ep.capturedEvents(), observability.SubjectGuardrailMatch)
	if len(matches) != 1 {
		t.Fatalf("want exactly one %s event, got %d", observability.SubjectGuardrailMatch, len(matches))
	}
	evt := matches[0]

	want := map[string]any{
		observability.AttrFerroGuardrailPlugin:   "regex-guard",
		observability.AttrFerroGuardrailInstance: "audit-hits",
		observability.AttrFerroGuardrailAction:   "log",
		observability.AttrFerroGuardrailStage:    "before_request",
		observability.AttrFerroGuardrailAllowed:  true,
	}
	for k, v := range want {
		if evt.Attributes[k] != v {
			t.Errorf("attribute %q = %v, want %v", k, evt.Attributes[k], v)
		}
	}

	// The gateway's no-leak posture: the event names the decision, never the
	// matched text or the pattern.
	for _, s := range eventStrings(evt) {
		if strings.Contains(s, canary) {
			t.Fatalf("guardrail-match event leaked the matched text %q in %q", canary, s)
		}
	}
}

// matchCapturingProvider opts into guardrail-match events.
type matchCapturingProvider struct{ eventCapturingProvider }

func (*matchCapturingProvider) GuardrailMatchesEnabled() bool { return true }

var _ observability.GuardrailMatchRecordingProvider = (*matchCapturingProvider)(nil)

// TestGateway_Route_GuardrailMatchIsOptIn holds the compatibility line: a
// provider written against "one Event per request" — it records events but never
// asked for match events — is handed none, exactly as with attempt events.
func TestGateway_Route_GuardrailMatchIsOptIn(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "mock"}},
		Plugins: []config.PluginConfig{{
			Name: "regex-guard", Type: "guardrail", Stage: "before_request", Enabled: true,
			Config: map[string]any{"action": "log", "rules": []any{map[string]any{"pattern": "tripwire"}}},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ep := &eventCapturingProvider{recordingActive: true} // no GuardrailMatchesEnabled
	gw.SetObservability(ep)
	if err := gw.LoadPlugins(); err != nil {
		t.Fatalf("LoadPlugins: %v", err)
	}
	gw.RegisterProvider(&mockProvider{name: "mock", models: []string{testModel},
		resp: &providers.Response{ID: "r1", Provider: "mock", Model: testModel}})

	if _, err := gw.Route(context.Background(), providers.Request{
		Model: testModel, Messages: []providers.Message{{Role: "user", Content: "a tripwire"}},
	}); err != nil {
		t.Fatalf("Route: %v", err)
	}

	events := ep.capturedEvents()
	if n := len(eventsWithSubject(events, observability.SubjectGuardrailMatch)); n != 0 {
		t.Fatalf("a provider that did not opt in received %d match events", n)
	}
	if n := len(eventsWithSubject(events, "gateway.request.completed")); n != 1 {
		t.Fatalf("want the one terminal event, got %d", n)
	}
}

// eventStrings returns every string-valued field and attribute of an event, so a
// test can assert none of them carries request content.
func eventStrings(evt observability.Event) []string {
	out := make([]string, 0, 7+2*len(evt.Attributes)+2*len(evt.Metadata))
	out = append(out, evt.Subject, evt.TraceID, evt.User, evt.SessionID, evt.Error, evt.Provider, evt.Model)
	for k, v := range evt.Attributes {
		out = append(out, k, fmt.Sprintf("%v", v))
	}
	for k, v := range evt.Metadata {
		out = append(out, k, v)
	}
	return out
}

// TestGateway_Passthrough_ObserveOnlyGuardrailRecordsUninspectableBody holds
// the pass-through surfaces to the routed ones: a body no guardrail can read is
// forwarded past an observe-only instance, and that instance records the
// decision its own action names, so log mode still counts what block mode would
// refuse. The pass-through never told the stage the body was unreadable, so the
// instance scanned an empty projection and recorded nothing at all.
func TestGateway_Passthrough_ObserveOnlyGuardrailRecordsUninspectableBody(t *testing.T) {
	guardrails := []struct {
		name   string
		plugin config.PluginConfig
		action string
	}{
		{
			name: "regex-guard log",
			plugin: config.PluginConfig{
				Name: "regex-guard", Type: "guardrail", Stage: "before_request", Enabled: true,
				Config: map[string]any{"action": "log", "rules": []any{map[string]any{"pattern": "tripwire"}}},
			},
			action: "log",
		},
		{
			name: "secret-scan warn",
			plugin: config.PluginConfig{
				Name: "secret-scan", Type: "guardrail", Stage: "before_request", Enabled: true,
				Config: map[string]any{"action": "warn"},
			},
			action: "warn",
		},
	}
	surfaces := []struct {
		name  string
		route func(gw *Gateway, forward func(context.Context) error) error
	}{
		{"/v1/*", func(gw *Gateway, forward func(context.Context) error) error {
			return gw.RoutePassthrough(context.Background(), "mock", testModel, "", false, forward)
		}},
		{"/v1/responses", func(gw *Gateway, forward func(context.Context) error) error {
			var usage providers.Usage
			return gw.RouteResponses(context.Background(), "mock", testModel, "", false, 0, &usage, forward)
		}},
	}
	for _, g := range guardrails {
		for _, s := range surfaces {
			t.Run(g.name+" on "+s.name, func(t *testing.T) {
				gw, err := newTestGateway(t, config.Config{
					Strategy: config.StrategyConfig{Mode: config.ModeSingle},
					Targets:  []config.Target{{VirtualKey: "mock"}},
					Plugins:  []config.PluginConfig{g.plugin},
				})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				ep := &matchCapturingProvider{eventCapturingProvider: eventCapturingProvider{recordingActive: true}}
				gw.SetObservability(ep)
				if err := gw.LoadPlugins(); err != nil {
					t.Fatalf("LoadPlugins: %v", err)
				}
				gw.RegisterProvider(&mockProvider{name: "mock", models: []string{testModel}})

				forwarded := false
				if err := s.route(gw, func(context.Context) error { forwarded = true; return nil }); err != nil {
					t.Fatalf("an observe-only guardrail must not refuse an unreadable body, got %v", err)
				}
				if !forwarded {
					t.Fatal("the body was not forwarded")
				}
				matches := eventsWithSubject(ep.capturedEvents(), observability.SubjectGuardrailMatch)
				if len(matches) != 1 {
					t.Fatalf("want one %s event for the unreadable body, got %d", observability.SubjectGuardrailMatch, len(matches))
				}
				if got := matches[0].Attributes[observability.AttrFerroGuardrailAction]; got != g.action {
					t.Errorf("recorded action = %v, want %q", got, g.action)
				}
			})
		}
	}
}
