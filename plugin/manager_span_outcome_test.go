package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/ferro-labs/ai-gateway/observability"
	"github.com/ferro-labs/ai-gateway/providers"
)

// A plugin's child span reports what THAT plugin did. Reject stays set once a
// request is denied, and the on_error stage runs precisely because something
// denied it, so a plugin there runs on a context already carrying another
// plugin's verdict. Its span must not report that verdict as its own — and an
// error the on_error plugin itself returns must still be recorded on its span.
func TestManager_OnErrorPluginSpanDoesNotInheritAnEarlierRejection(t *testing.T) {
	broke := errors.New("request log store unavailable")

	tests := []struct {
		name        string
		execFn      func(ctx context.Context, pctx *Context) error
		wantOutcome string
		wantErr     bool
	}{
		{name: "an on_error plugin that succeeds", wantOutcome: "ok"},
		{
			name:        "an on_error plugin that fails",
			execFn:      func(context.Context, *Context) error { return broke },
			wantOutcome: "error",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManager(nil)
			if err := m.Register(StageBeforeRequest, &mockPlugin{
				name: "rate-limit",
				typ:  TypeRateLimit,
				execFn: func(_ context.Context, pctx *Context) error {
					pctx.Reject = true
					pctx.Reason = "rate limit exceeded"
					return nil
				},
			}); err != nil {
				t.Fatalf("register rate-limit: %v", err)
			}
			if err := m.Register(StageOnError, &mockPlugin{
				name:   "request-logger",
				typ:    TypeLogging,
				execFn: tt.execFn,
			}); err != nil {
				t.Fatalf("register request-logger: %v", err)
			}

			root := &recordingSpan{}
			pctx := NewContext(&providers.Request{Model: "gpt-4o"})
			pctx.Span = root

			var rejection *RejectionError
			if err := m.RunBefore(context.Background(), pctx); !errors.As(err, &rejection) {
				t.Fatalf("RunBefore error = %v, want the rate limiter's rejection", err)
			}

			root.mu.Lock()
			children := append([]*recordingSpan(nil), root.children...)
			root.mu.Unlock()
			if len(children) != 2 {
				t.Fatalf("expected 2 plugin spans, got %d", len(children))
			}

			denier, observer := children[0], children[1]
			if got := denier.attrs[observability.AttrFerroPluginOutcome]; got != "rejected" {
				t.Errorf("rate-limit span outcome = %v, want rejected", got)
			}
			if observer.name != "plugin.on_error.request-logger" {
				t.Fatalf("second span = %q, want the on_error request-logger span", observer.name)
			}
			if got := observer.attrs[observability.AttrFerroPluginOutcome]; got != tt.wantOutcome {
				t.Errorf("request-logger span outcome = %v, want %s: it denied nothing", got, tt.wantOutcome)
			}
			if got, ok := observer.attrs[observability.AttrFerroPluginReason]; ok {
				t.Errorf("request-logger span carries %s = %v, another plugin's reason", observability.AttrFerroPluginReason, got)
			}
			if tt.wantErr && !errors.Is(observer.err, broke) {
				t.Errorf("request-logger span error = %v, want its own failure %v", observer.err, broke)
			}
			if !tt.wantErr && observer.err != nil {
				t.Errorf("request-logger span error = %v, want none", observer.err)
			}
		})
	}
}
