package aigateway

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/pkg/circuitbreaker"
	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestGateway_RouteStream_StartPastGatewayDeadlineCountsAgainstBreaker covers
// a streaming upstream that accepts the connection and never answers. The
// start is abandoned when the gateway's own deadline elapses — request_timeout
// or targets[].timeout — and that is the provider being too slow, exactly as it
// is on the unary surfaces. It must count toward opening the circuit; a hung
// streaming target otherwise stays in rotation forever while /readyz, whose
// only provider signal is circuit state, keeps reporting it routable.
//
// The caller's own deadline is the other case: it says nothing about the
// provider and must not count.
func TestGateway_RouteStream_StartPastGatewayDeadlineCountsAgainstBreaker(t *testing.T) {
	breaker := &config.CircuitBreakerConfig{FailureThreshold: 1, SuccessThreshold: 1, MaxHalfThreshold: 1, Timeout: "1h"}
	tests := []struct {
		name           string
		requestTimeout string
		targetTimeout  string
		callerTimeout  time.Duration
		wantState      circuitbreaker.State
	}{
		{name: "request_timeout", requestTimeout: "50ms", wantState: circuitbreaker.StateOpen},
		{name: "targets[].timeout", targetTimeout: "50ms", wantState: circuitbreaker.StateOpen},
		{name: "caller deadline", callerTimeout: 50 * time.Millisecond, wantState: circuitbreaker.StateClosed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })

			gw, err := newTestGateway(t, config.Config{
				Strategy:       config.StrategyConfig{Mode: config.ModeSingle},
				RequestTimeout: tt.requestTimeout,
				Targets: []config.Target{{
					VirtualKey:     mockProviderName,
					Timeout:        tt.targetTimeout,
					CircuitBreaker: breaker,
				}},
			})
			if err != nil {
				t.Fatalf("new gateway: %v", err)
			}
			gw.RegisterProvider(&mockStreamProvider{
				mockProvider: mockProvider{name: mockProviderName, models: []string{"gpt-4o"}},
				streamFn: func(ctx context.Context, _ providers.Request) (<-chan providers.StreamChunk, error) {
					select {
					case <-release:
					case <-ctx.Done():
					}
					return nil, errors.New("hung start released")
				},
			})
			cb := gw.circuitBreakerFor(t, mockProviderName)

			ctx := context.Background()
			if tt.callerTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.callerTimeout)
				defer cancel()
			}
			if _, err := gw.RouteStream(ctx, streamTestRequest()); err == nil {
				t.Fatal("a stream start that never answered succeeded")
			}
			if got := cb.State(); got != tt.wantState {
				t.Errorf("breaker state = %v, want %v", got, tt.wantState)
			}
		})
	}
}

// TestGateway_RouteStream_AbandonedStartHoldingTheOnlySlot covers the
// abandoned start together with max_concurrency 1, where the breaker admission
// and the concurrency slot have different owners and different lifetimes.
//
// The abandoned start keeps running, and keeps the target's only slot, until
// the provider answers. Its admission is resolved when the wait is abandoned —
// once — so its late answer must not resolve anything again. A probe that then
// queues behind that slot never reached the provider: its wait ending in the
// queue is a shed, not a provider failure, so it must release its probe rather
// than reopen the circuit, and must leave the queue rather than call the
// provider later for nobody. The slot must come back exactly once, when the
// abandoned stream is drained.
func TestGateway_RouteStream_AbandonedStartHoldingTheOnlySlot(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy:       config.StrategyConfig{Mode: config.ModeSingle},
		RequestTimeout: "50ms",
		Targets: []config.Target{{
			VirtualKey: mockProviderName,
			CircuitBreaker: &config.CircuitBreakerConfig{
				FailureThreshold: 1,
				SuccessThreshold: 1,
				MaxHalfThreshold: 1,
				Timeout:          "30s",
			},
			Concurrency: &config.ConcurrencyConfig{MaxConcurrency: 1, QueueSize: 1},
		}},
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}

	var calls atomic.Int32
	releaseHung := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseHung) }) }
	t.Cleanup(release)
	gw.RegisterProvider(&mockStreamProvider{
		mockProvider: mockProvider{name: mockProviderName, models: []string{"gpt-4o"}},
		streamFn: func(context.Context, providers.Request) (<-chan providers.StreamChunk, error) {
			if calls.Add(1) == 1 {
				// Overruns request_timeout, then answers with a live stream
				// nobody will read.
				<-releaseHung
			}
			return chunkStream("gpt-4o"), nil
		},
	})
	cb, lim := resilienceFor(t, gw, mockProviderName)
	if cb == nil || lim == nil {
		t.Fatal("expected a circuit breaker and a limiter for the target")
	}
	advance := virtualClock(cb)
	held := func() int { return len(lim.slots) }

	// The start overruns the deadline while holding the only slot.
	if _, err := gw.RouteStream(context.Background(), streamTestRequest()); !errors.Is(err, ErrRequestTimeout) {
		t.Fatalf("hung start = %v, want the gateway's request timeout", err)
	}
	if got := cb.State(); got != circuitbreaker.StateOpen {
		t.Fatalf("breaker state after the hung start = %v, want open: overrunning the gateway's own deadline is the provider's failure", got)
	}
	if held() != 1 {
		t.Fatalf("slots held = %d, want 1: the abandoned start still occupies the target", held())
	}

	// The circuit ages into half-open. The probe it admits queues behind the
	// abandoned start's slot until its own deadline.
	advance(time.Minute)
	_, err = gw.RouteStream(context.Background(), streamTestRequest())
	if !errors.Is(err, providers.ErrProviderSaturated) {
		t.Errorf("probe queued behind the held slot = %v, want ErrProviderSaturated", err)
	}
	if got := cb.State(); got != circuitbreaker.StateHalfOpen {
		t.Errorf("breaker state after the queued probe = %v, want half_open: a shed under our own concurrency limit is not the provider's failure", got)
	}

	// The abandoned start answers late. Draining it frees the slot, once, and
	// its answer resolves nothing on the breaker.
	release()
	waitFor(t, func() bool { return held() == 0 })
	if got := cb.State(); got != circuitbreaker.StateHalfOpen {
		t.Errorf("breaker state after the abandoned start answered = %v, want half_open: its admission was already resolved", got)
	}

	// The circuit still has its probe slot: a fresh probe is admitted, takes
	// the freed concurrency slot, reaches the provider and closes the circuit.
	ch, err := gw.RouteStream(context.Background(), streamTestRequest())
	if err != nil {
		t.Fatalf("probe after the slot was freed: %v", err)
	}
	drainMeteredStream(t, ch)
	if got := cb.State(); got != circuitbreaker.StateClosed {
		t.Errorf("breaker state after the probe stream succeeded = %v, want closed", got)
	}
	waitFor(t, func() bool { return held() == 0 })
	if got := calls.Load(); got != 2 {
		t.Errorf("provider stream starts = %d, want 2: the probe shed from the queue must never reach the provider", got)
	}
}

// TestGateway_RouteStream_NilStreamCountsAgainstBreaker covers a provider that
// answers a stream start with neither a channel nor an error. The start is
// refused as a failure, so its half-open probe must be resolved as one: left
// held, the circuit stays half-open at its probe cap and rejects the target
// until the process restarts. Behind a concurrency limit the nil channel must
// not be handed on as a stream that never ends, holding the slot with it.
func TestGateway_RouteStream_NilStreamCountsAgainstBreaker(t *testing.T) {
	for _, limited := range []bool{false, true} {
		name := "unlimited"
		if limited {
			name = "max_concurrency"
		}
		t.Run(name, func(t *testing.T) {
			target := config.Target{
				VirtualKey: mockProviderName,
				CircuitBreaker: &config.CircuitBreakerConfig{
					FailureThreshold: 1,
					SuccessThreshold: 1,
					MaxHalfThreshold: 1,
					Timeout:          "30s",
				},
			}
			if limited {
				target.Concurrency = &config.ConcurrencyConfig{MaxConcurrency: 1, QueueSize: 1}
			}
			gw, err := newTestGateway(t, config.Config{
				Strategy: config.StrategyConfig{Mode: config.ModeSingle},
				Targets:  []config.Target{target},
			})
			if err != nil {
				t.Fatalf("new gateway: %v", err)
			}
			var calls atomic.Int32
			gw.RegisterProvider(&mockStreamProvider{
				mockProvider: mockProvider{name: mockProviderName, models: []string{"gpt-4o"}},
				streamFn: func(context.Context, providers.Request) (<-chan providers.StreamChunk, error) {
					switch calls.Add(1) {
					case 1:
						return nil, core.StatusError(mockProviderName, http.StatusServiceUnavailable, "down")
					case 2:
						return nil, nil
					default:
						return chunkStream("gpt-4o"), nil
					}
				},
			})
			cb, lim := resilienceFor(t, gw, mockProviderName)
			advance := virtualClock(cb)

			if _, err := gw.RouteStream(context.Background(), streamTestRequest()); err == nil {
				t.Fatal("the failing start succeeded")
			}
			advance(time.Minute)

			// Bounded, so a nil channel handed on as a stream cannot hang the test.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := gw.RouteStream(ctx, streamTestRequest()); err == nil {
				t.Fatal("a nil stream was served as a stream")
			}
			if got := cb.State(); got != circuitbreaker.StateOpen {
				t.Fatalf("breaker state after a nil stream = %v, want open: the half-open probe was never resolved", got)
			}
			if lim != nil && len(lim.slots) != 0 {
				t.Errorf("slots held after a nil stream = %d, want 0", len(lim.slots))
			}

			advance(time.Minute)
			ch, err := gw.RouteStream(context.Background(), streamTestRequest())
			if err != nil {
				t.Fatalf("probe after the circuit aged back into half-open: %v", err)
			}
			drainMeteredStream(t, ch)
			if got := cb.State(); got != circuitbreaker.StateClosed {
				t.Errorf("breaker state after a healthy probe = %v, want closed", got)
			}
		})
	}
}
