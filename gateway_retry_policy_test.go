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

// A retry block belongs to the target that declares it: under fallback the
// first target, configured without one, is asked once, and the second is asked
// as many times as its own block says.
func TestRetryPolicy_IsResolvedPerTarget(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeFallback},
		Targets: []config.Target{
			{VirtualKey: "first"},
			{VirtualKey: "second", Retry: &config.RetryConfig{Attempts: 3, InitialBackoffMs: 1}},
		},
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	down := func() (*providers.Response, error) {
		return nil, core.StatusError("upstream", http.StatusServiceUnavailable, "down")
	}
	first := newCountingProvider("first", down)
	second := newCountingProvider("second", down)
	gw.RegisterProvider(first)
	gw.RegisterProvider(second)

	if _, err := gw.Route(context.Background(), pipelineRequest()); err == nil {
		t.Fatal("Route succeeded with every target down")
	}
	if first.calls.Load() != 1 || second.calls.Load() != 3 {
		t.Fatalf("first=%d second=%d calls, want 1 and 3: each target follows its own retry block",
			first.calls.Load(), second.calls.Load())
	}
}

// on_status_codes limits retries to the listed statuses: a listed status is
// retried up to attempts, an unlisted one is not retried at all.
func TestRetryPolicy_OnStatusCodesLimitsRetries(t *testing.T) {
	for _, tc := range []struct {
		status    int
		wantCalls int64
	}{
		{http.StatusBadGateway, 3},
		{http.StatusServiceUnavailable, 1},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			gw, err := newTestGateway(t, config.Config{
				Strategy: config.StrategyConfig{Mode: config.ModeSingle},
				Targets: []config.Target{{VirtualKey: "only", Retry: &config.RetryConfig{
					Attempts: 3, InitialBackoffMs: 1, OnStatusCodes: []int{http.StatusBadGateway},
				}}},
			})
			if err != nil {
				t.Fatalf("new gateway: %v", err)
			}
			p := newCountingProvider("only", func() (*providers.Response, error) {
				return nil, core.StatusError("upstream", tc.status, "failed")
			})
			gw.RegisterProvider(p)

			if _, err := gw.Route(context.Background(), pipelineRequest()); err == nil {
				t.Fatal("Route succeeded against a failing target")
			}
			if got := p.calls.Load(); got != tc.wantCalls {
				t.Fatalf("calls = %d, want %d with on_status_codes [502]", got, tc.wantCalls)
			}
		})
	}
}

// initial_backoff_ms is the wait the retry draws from. A configured wait no
// request deadline can accommodate abandons the target rather than retrying it
// early: the default 100 ms backoff would have retried well inside the deadline.
func TestRetryPolicy_HonoursInitialBackoff(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets: []config.Target{{VirtualKey: "only", Retry: &config.RetryConfig{
			Attempts: 2, InitialBackoffMs: 1_000_000_000, // ~11.6 days, drawn with full jitter
		}}},
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	p := newCountingProvider("only", func() (*providers.Response, error) {
		return nil, core.StatusError("upstream", http.StatusServiceUnavailable, "down")
	})
	gw.RegisterProvider(p)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := gw.Route(ctx, pipelineRequest()); err == nil {
		t.Fatal("Route succeeded against a failing target")
	}
	if got := p.calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1: the configured backoff cannot end before the request deadline", got)
	}
}

// An attempt timeout is retried as a transport failure is because it carries
// no status. A target that answered a deterministic 4xx as its attempt
// deadline fired has answered: that status is final, retry block or not.
func TestRetryPolicy_AStatusAtTheAttemptDeadlineIsNotATimeoutRetry(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets: []config.Target{{VirtualKey: "only", Timeout: "20ms", Retry: &config.RetryConfig{
			Attempts: 3, InitialBackoffMs: 1,
		}}},
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	p := &countingProvider{}
	p.name = "only"
	p.models = []string{pipelineModel}
	p.completeFn = func(ctx context.Context, _ providers.Request) (*providers.Response, error) {
		p.calls.Add(1)
		<-ctx.Done()
		return nil, core.StatusError("only", http.StatusBadRequest, "invalid request")
	}
	gw.RegisterProvider(p)

	if _, err := gw.Route(context.Background(), pipelineRequest()); err == nil {
		t.Fatal("Route succeeded against a target that answered 400")
	}
	if got := p.calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1: a 400 is not retried because it arrived at the attempt deadline", got)
	}
}
