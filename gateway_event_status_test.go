package aigateway

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

// A denial is not a server error, on the wire or off the box.
//
// A rate-limit rejection answers the caller 429 and is counted as a rejection
// by the gateway's own metrics, while the event shipped to observability
// exporters said 500 — so an exporter's error rate disagreed with both, and a
// throttled client looked like a broken gateway to whoever was watching.
func TestGateway_FailedEventCarriesTheCallerStatus(t *testing.T) {
	tests := []struct {
		name       string
		pluginType plugin.PluginType
		want       int
	}{
		{"rate limit denial", plugin.TypeRateLimit, http.StatusTooManyRequests},
		{"guardrail denial", plugin.TypeGuardrail, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw, _ := newTestGateway(t, config.Config{
				Strategy: config.StrategyConfig{Mode: config.ModeSingle},
				Targets:  []config.Target{{VirtualKey: mockProviderName}},
			})

			ep := &eventCapturingProvider{recordingActive: true}
			gw.SetObservability(ep)

			gw.RegisterProvider(&mockEmbeddingProvider{
				mockProvider: mockProvider{name: mockProviderName, models: []string{testModel}},
			})
			_ = gw.RegisterPlugin(plugin.StageBeforeRequest, &testPlugin{
				name: "denier",
				typ:  tt.pluginType,
				execFn: func(_ context.Context, pctx *plugin.Context) error {
					pctx.Reject = true
					pctx.Reason = "over the limit"
					return nil
				},
			})

			if _, err := gw.Embed(context.Background(), providers.EmbeddingRequest{
				Model: testModel,
				Input: "hi",
			}); err == nil {
				t.Fatal("expected the plugin denial to abort the request")
			}

			evts := ep.capturedEvents()
			if len(evts) != 1 {
				t.Fatalf("expected 1 event, got %d: %v", len(evts), evts)
			}
			if got := evts[0].Status; got != tt.want {
				t.Errorf("exporter event Status = %d, want %d — the status the caller was given", got, tt.want)
			}
		})
	}
}

// A chat request a plugin denies is recorded like a denial on every other
// surface: one failed lifecycle event carrying the caller's status, and the
// failure on the root span. Chat sent no event at all — on either path — so a
// throttled or blocked chat request never reached an exporter or an event hook,
// and a denied stream's span read as a success.
func TestGateway_ChatDenialIsRecordedAsAFailedRequest(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "route", true: "stream"}[stream], func(t *testing.T) {
			gw, _ := newTestGateway(t, config.Config{
				Strategy: config.StrategyConfig{Mode: config.ModeSingle},
				Targets:  []config.Target{{VirtualKey: mockProviderName}},
			})

			ep := &eventCapturingProvider{recordingActive: true}
			gw.SetObservability(ep)

			gw.RegisterProvider(&mockStreamProvider{
				mockProvider: mockProvider{name: mockProviderName, models: []string{testModel}, resp: &providers.Response{ID: "unreached"}},
			})
			_ = gw.RegisterPlugin(plugin.StageBeforeRequest, &testPlugin{
				name: "limiter",
				typ:  plugin.TypeRateLimit,
				execFn: func(_ context.Context, pctx *plugin.Context) error {
					pctx.Reject = true
					pctx.Reason = "over the limit"
					return nil
				},
			})

			req := providers.Request{Model: testModel, Stream: stream, Messages: []providers.Message{{Role: "user", Content: "hi"}}}
			var err error
			if stream {
				_, err = gw.RouteStream(context.Background(), req)
			} else {
				_, err = gw.Route(context.Background(), req)
			}
			if err == nil {
				t.Fatal("expected the plugin denial to abort the request")
			}

			if span := ep.rootSpan(); span == nil || span.err == nil {
				t.Error("the denied request's root span carries no error, so the trace reads as a success")
			}
			failed := eventsWithSubject(ep.capturedEvents(), "gateway.request.failed")
			if len(failed) != 1 {
				t.Fatalf("failed events = %d, want 1 for the denied request", len(failed))
			}
			if got := failed[0].Status; got != http.StatusTooManyRequests {
				t.Errorf("failed event Status = %d, want %d — the status the caller was given", got, http.StatusTooManyRequests)
			}
			if failed[0].Stream != stream {
				t.Errorf("failed event Stream = %v, want %v", failed[0].Stream, stream)
			}
		})
	}
}

// A request an after_request plugin ends reaches the event hooks as failed,
// streamed or not. The non-streaming path always published it; a stream
// published nothing — not completed, not failed — so a hook that counts or
// audits requests missed every stream an after_request guardrail rejected.
func TestGateway_AfterRequestRejectionReachesEventHooks(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "route", true: "stream"}[stream], func(t *testing.T) {
			gw, _ := newTestGateway(t, config.Config{
				Strategy: config.StrategyConfig{Mode: config.ModeSingle},
				Targets:  []config.Target{{VirtualKey: mockProviderName}},
			})
			type hookCall struct {
				subject string
				data    map[string]any
			}
			calls := make(chan hookCall, 4)
			gw.AddHook(func(_ context.Context, subject string, data map[string]any) {
				calls <- hookCall{subject, data}
			})
			gw.RegisterProvider(&mockStreamProvider{
				mockProvider: mockProvider{name: mockProviderName, models: []string{testModel}, resp: &providers.Response{
					ID:      "answer",
					Choices: []providers.Choice{{Message: providers.Message{Role: "assistant", Content: "the secret is hunter2"}}},
				}},
				streamFn: func(context.Context, providers.Request) (<-chan providers.StreamChunk, error) {
					ch := make(chan providers.StreamChunk, 1)
					ch <- providers.StreamChunk{Choices: []providers.StreamChoice{{
						Delta:        providers.MessageDelta{Content: "the secret is hunter2"},
						FinishReason: "stop",
					}}}
					close(ch)
					return ch, nil
				},
			})
			_ = gw.RegisterPlugin(plugin.StageAfterRequest, &testPlugin{
				name: "secret-scan",
				typ:  plugin.TypeGuardrail,
				execFn: func(_ context.Context, pctx *plugin.Context) error {
					pctx.Reject = true
					pctx.Reason = "secret in response"
					return nil
				},
			})

			req := providers.Request{Model: testModel, Stream: stream, Messages: []providers.Message{{Role: "user", Content: "hi"}}}
			if stream {
				ch, err := gw.RouteStream(context.Background(), req)
				if err != nil {
					t.Fatalf("RouteStream error = %v", err)
				}
				for range ch { //nolint:revive // empty-block: draining the stream to completion
				}
			} else if _, err := gw.Route(context.Background(), req); err == nil {
				t.Fatal("expected the after_request rejection to fail the request")
			}

			select {
			case call := <-calls:
				if call.subject != SubjectRequestFailed {
					t.Fatalf("hook subject = %q, want %q", call.subject, SubjectRequestFailed)
				}
				if got := call.data["stream"]; got != stream {
					t.Errorf("hook stream = %v, want %v", got, stream)
				}
				if got := call.data["status"]; got != http.StatusBadGateway {
					t.Errorf("hook status = %v, want %d", got, http.StatusBadGateway)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no event hook ran for the rejected request")
			}
		})
	}
}
