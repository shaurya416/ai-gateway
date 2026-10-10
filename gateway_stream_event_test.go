package aigateway

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/streamio"
	"github.com/ferro-labs/ai-gateway/pkg/metrics"
	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// A stream that fails reaches observability exporters with the status the
// failure classifies as, the same one event hooks and a non-streamed request
// report. The exporter event was rebuilt from the error's message alone, which
// drops its type, so every stream failure — an after_request guardrail's 502,
// an upstream's mid-stream error — reached exporters as a 500.
func TestGateway_RouteStream_FailedExporterEventCarriesTheStatus(t *testing.T) {
	tests := []struct {
		name     string
		chunks   []providers.StreamChunk
		rejectAt plugin.Stage
	}{
		{
			name: "after_request rejection",
			chunks: []providers.StreamChunk{{Choices: []providers.StreamChoice{{
				Delta:        providers.MessageDelta{Content: "the secret is hunter2"},
				FinishReason: "stop",
			}}}},
			rejectAt: plugin.StageAfterRequest,
		},
		{
			name: "upstream error mid-stream",
			chunks: []providers.StreamChunk{
				{Choices: []providers.StreamChoice{{Delta: providers.MessageDelta{Content: "partial"}}}},
				{Error: &core.HTTPStatusError{StatusCode: http.StatusServiceUnavailable, Message: "overloaded"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw, _ := newTestGateway(t, config.Config{
				Strategy: config.StrategyConfig{Mode: config.ModeSingle},
				Targets:  []config.Target{{VirtualKey: mockProviderName}},
			})
			ep := &eventCapturingProvider{recordingActive: true}
			gw.SetObservability(ep)
			hookStatus := make(chan any, 4)
			gw.AddHook(func(_ context.Context, subject string, data map[string]any) {
				if subject == SubjectRequestFailed {
					hookStatus <- data["status"]
				}
			})
			gw.RegisterProvider(&mockStreamProvider{
				mockProvider: mockProvider{name: mockProviderName, models: []string{testModel}},
				streamFn: func(context.Context, providers.Request) (<-chan providers.StreamChunk, error) {
					ch := make(chan providers.StreamChunk, len(tt.chunks))
					for _, c := range tt.chunks {
						ch <- c
					}
					close(ch)
					return ch, nil
				},
			})
			if tt.rejectAt != "" {
				_ = gw.RegisterPlugin(tt.rejectAt, &testPlugin{
					name: "secret-scan",
					typ:  plugin.TypeGuardrail,
					execFn: func(_ context.Context, pctx *plugin.Context) error {
						pctx.Reject = true
						pctx.Reason = "secret in response"
						return nil
					},
				})
			}

			ch, err := gw.RouteStream(context.Background(), providers.Request{
				Model:    testModel,
				Stream:   true,
				Messages: []providers.Message{{Role: "user", Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("RouteStream error = %v", err)
			}
			for range ch { //nolint:revive // empty-block: draining the stream to completion
			}

			failed := eventsWithSubject(ep.capturedEvents(), SubjectRequestFailed)
			if len(failed) != 1 {
				t.Fatalf("failed exporter events = %d, want 1", len(failed))
			}
			if got := failed[0].Status; got != http.StatusBadGateway {
				t.Errorf("exporter event Status = %d, want %d", got, http.StatusBadGateway)
			}
			select {
			case got := <-hookStatus:
				if got != failed[0].Status {
					t.Errorf("hook status = %v, exporter status = %d; one failed stream reported two ways", got, failed[0].Status)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no event hook ran for the failed stream")
			}
		})
	}
}

// A stream that fails before its first chunk still took the time it took. The
// exporter event measured latency to the last chunk received, which for a
// stream that received none is zero, so a stream abandoned after waiting on a
// silent upstream was reported to exporters as failing instantly — and, its
// error rebuilt from the message, as a 500 rather than the idle bound's 504.
func TestGateway_RouteStream_FailedExporterEventCarriesTheLatency(t *testing.T) {
	gw, _ := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: mockProviderName}},
	})
	ep := &eventCapturingProvider{recordingActive: true}
	gw.SetObservability(ep)
	gw.RegisterProvider(&mockStreamProvider{
		mockProvider: mockProvider{name: mockProviderName, models: []string{testModel}},
		streamFn: func(ctx context.Context, _ providers.Request) (<-chan providers.StreamChunk, error) {
			ch := make(chan providers.StreamChunk)
			go func() {
				<-ctx.Done()
				close(ch)
			}()
			return ch, nil
		},
	})

	const wait = 80 * time.Millisecond
	// Cancelled the way the SSE writer cancels a stream whose upstream went
	// silent: with the idle bound as the cause.
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	ch, err := gw.RouteStream(ctx, providers.Request{
		Model:    testModel,
		Stream:   true,
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("RouteStream error = %v", err)
	}
	time.Sleep(wait)
	cancel(streamio.ErrIdleTimeout)
	for range ch { //nolint:revive // empty-block: draining the stream to completion
	}

	failed := eventsWithSubject(ep.capturedEvents(), SubjectRequestFailed)
	if len(failed) != 1 {
		t.Fatalf("failed exporter events = %d, want 1", len(failed))
	}
	if got := failed[0].LatencyMs; got < wait.Milliseconds() {
		t.Errorf("exporter event LatencyMs = %d, want at least %d — the time the stream ran before it failed", got, wait.Milliseconds())
	}
	if got := failed[0].Status; got != http.StatusGatewayTimeout {
		t.Errorf("exporter event Status = %d, want %d for the idle bound", got, http.StatusGatewayTimeout)
	}
}

// An after_request guardrail that denies a response is counted as a
// rejection, streamed or not — the plugin failure policy keeps a plugin that
// denies apart from one that breaks. A stream counted the denial as a failed
// request and as a provider error against the target that served it, so a
// guardrail doing its job read as the provider failing.
func TestGateway_AfterRequestRejectionIsCountedAsARejection(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "route", true: "stream"}[stream], func(t *testing.T) {
			name := "after-reject-metrics-" + map[bool]string{false: "route", true: "stream"}[stream]
			gw, _ := newTestGateway(t, config.Config{
				Strategy: config.StrategyConfig{Mode: config.ModeSingle},
				Targets:  []config.Target{{VirtualKey: name}},
			})
			gw.RegisterProvider(&mockStreamProvider{
				mockProvider: mockProvider{name: name, models: []string{testModel}, resp: &providers.Response{
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

			handles := metrics.ForRequest(name, gw.metricModel(testModel))
			rejectedBefore := counterValue(t, handles.Rejected)
			errorBefore := counterValue(t, handles.Error)
			pluginErrBefore := counterValue(t, metrics.ForProviderError(name, metrics.ErrTypePlugin))

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

			if got := counterValue(t, handles.Rejected) - rejectedBefore; got != 1 {
				t.Errorf("requests_total{status=rejected} grew by %v, want 1", got)
			}
			if got := counterValue(t, handles.Error) - errorBefore; got != 0 {
				t.Errorf("requests_total{status=error} grew by %v, want 0 — a denial is not a failure", got)
			}
			if got := counterValue(t, metrics.ForProviderError(name, metrics.ErrTypePlugin)) - pluginErrBefore; got != 0 {
				t.Errorf("provider_errors_total{error_type=plugin_error} grew by %v, want 0 — the provider served the request", got)
			}
		})
	}
}
