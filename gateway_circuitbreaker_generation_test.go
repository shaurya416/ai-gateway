package aigateway

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/pkg/circuitbreaker"
	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// generationBreakerConfig is a single target whose breaker opens on one
// failure, admits one half-open probe, and closes on one success.
func generationBreakerConfig() config.Config {
	return config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets: []config.Target{{
			VirtualKey: mockProviderName,
			CircuitBreaker: &config.CircuitBreakerConfig{
				FailureThreshold: 1,
				SuccessThreshold: 1,
				MaxHalfThreshold: 1,
				Timeout:          "30s",
			},
		}},
	}
}

// virtualClock drives a breaker's open timeout from the test. It is read from
// the request goroutines, so it is atomic.
func virtualClock(cb *circuitbreaker.CircuitBreaker) func(time.Duration) {
	var nanos atomic.Int64
	cb.SetNowForTest(func() time.Time { return time.Unix(0, nanos.Load()) })
	return func(d time.Duration) { nanos.Add(int64(d)) }
}

// TestGateway_Route_PreOutageSuccessDoesNotCloseHalfOpenCircuit covers a call
// that outlives two state transitions. It was admitted while the circuit was
// closed, the circuit then opened and aged into half-open, and only then did it
// answer. Its success is evidence about the provider before the outage, not a
// probe's verdict on recovery: it must not close the circuit, and it must not
// free the probe slot the real probe still holds — which would admit a second
// probe past max_half_threshold.
func TestGateway_Route_PreOutageSuccessDoesNotCloseHalfOpenCircuit(t *testing.T) {
	gw, err := newTestGateway(t, generationBreakerConfig())
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}

	var calls atomic.Int32
	entered := make(chan int32, 4)
	releaseSlow := make(chan struct{})
	releaseProbe := make(chan struct{})
	gw.RegisterProvider(&mockProvider{
		name:   mockProviderName,
		models: []string{pipelineModel},
		completeFn: func(context.Context, providers.Request) (*providers.Response, error) {
			n := calls.Add(1)
			entered <- n
			switch n {
			case 1: // admitted while closed; answers after the outage began
				<-releaseSlow
				return &providers.Response{ID: "pre-outage", Model: pipelineModel}, nil
			case 2: // trips the breaker
				return nil, core.StatusError(mockProviderName, http.StatusServiceUnavailable, "down")
			case 3: // the half-open probe
				<-releaseProbe
				return &providers.Response{ID: "probe", Model: pipelineModel}, nil
			default:
				return &providers.Response{ID: "late", Model: pipelineModel}, nil
			}
		},
	})
	cb := gw.circuitBreakerFor(t, mockProviderName)
	advance := virtualClock(cb)

	slowDone := make(chan error, 1)
	go func() {
		_, err := gw.Route(context.Background(), pipelineRequest())
		slowDone <- err
	}()
	<-entered

	if _, err := gw.Route(context.Background(), pipelineRequest()); err == nil {
		t.Fatal("the failing call succeeded")
	}
	<-entered
	if got := cb.State(); got != circuitbreaker.StateOpen {
		t.Fatalf("breaker state after the failure = %v, want open", got)
	}

	advance(time.Minute)
	probeDone := make(chan error, 1)
	go func() {
		_, err := gw.Route(context.Background(), pipelineRequest())
		probeDone <- err
	}()
	<-entered

	// The pre-outage call answers now, while the probe is still in flight.
	close(releaseSlow)
	if err := <-slowDone; err != nil {
		t.Fatalf("pre-outage call: %v", err)
	}

	if got := cb.State(); got != circuitbreaker.StateHalfOpen {
		t.Errorf("breaker state = %v, want half_open: a call admitted before the outage closed the circuit while its probe was still undecided", got)
	}
	if _, err := gw.Route(context.Background(), pipelineRequest()); !errors.Is(err, circuitbreaker.ErrCircuitOpen) {
		t.Errorf("request while the only probe is in flight = %v, want ErrCircuitOpen: the pre-outage call freed a probe slot it never took", err)
	}

	close(releaseProbe)
	if err := <-probeDone; err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got := cb.State(); got != circuitbreaker.StateClosed {
		t.Errorf("breaker state after the probe succeeded = %v, want closed", got)
	}
}

// TestGateway_RouteStream_PreOutageStreamDoesNotCloseHalfOpenCircuit is the
// streaming form, and the likelier one: a stream's outcome is resolved when it
// ENDS, and a long generation easily outlasts a breaker's open timeout. A
// stream that began before the outage and finishes cleanly after the circuit
// aged into half-open must leave the half-open decision to the probe.
func TestGateway_RouteStream_PreOutageStreamDoesNotCloseHalfOpenCircuit(t *testing.T) {
	gw, err := newTestGateway(t, generationBreakerConfig())
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}

	var calls atomic.Int32
	entered := make(chan int32, 4)
	finishLong := make(chan struct{})
	releaseProbe := make(chan struct{})
	gw.RegisterProvider(&mockStreamProvider{
		mockProvider: mockProvider{name: mockProviderName, models: []string{"gpt-4o"}},
		streamFn: func(context.Context, providers.Request) (<-chan providers.StreamChunk, error) {
			n := calls.Add(1)
			entered <- n
			switch n {
			case 1: // starts while closed; ends after the outage began
				ch := make(chan providers.StreamChunk)
				go func() {
					defer close(ch)
					<-finishLong
					ch <- providers.StreamChunk{ID: "pre-outage", Model: "gpt-4o"}
				}()
				return ch, nil
			case 2: // trips the breaker
				return nil, core.StatusError(mockProviderName, http.StatusServiceUnavailable, "down")
			case 3: // the half-open probe
				<-releaseProbe
				return chunkStream("gpt-4o"), nil
			default:
				return chunkStream("gpt-4o"), nil
			}
		},
	})
	cb := gw.circuitBreakerFor(t, mockProviderName)
	advance := virtualClock(cb)

	long, err := gw.RouteStream(context.Background(), streamTestRequest())
	if err != nil {
		t.Fatalf("long stream start: %v", err)
	}
	<-entered

	if _, err := gw.RouteStream(context.Background(), streamTestRequest()); err == nil {
		t.Fatal("the failing start succeeded")
	}
	<-entered
	if got := cb.State(); got != circuitbreaker.StateOpen {
		t.Fatalf("breaker state after the failure = %v, want open", got)
	}

	advance(time.Minute)
	type startResult struct {
		ch  <-chan providers.StreamChunk
		err error
	}
	probeDone := make(chan startResult, 1)
	go func() {
		ch, err := gw.RouteStream(context.Background(), streamTestRequest())
		probeDone <- startResult{ch, err}
	}()
	<-entered

	// The pre-outage stream ends cleanly now, while the probe is in flight.
	// The metering goroutine closes the wrapped channel only after resolving
	// the breaker outcome, so the drain returning means it has landed.
	close(finishLong)
	drainMeteredStream(t, long)

	if got := cb.State(); got != circuitbreaker.StateHalfOpen {
		t.Errorf("breaker state = %v, want half_open: a stream started before the outage closed the circuit while its probe was still undecided", got)
	}

	close(releaseProbe)
	probe := <-probeDone
	if probe.err != nil {
		t.Fatalf("probe start: %v", probe.err)
	}
	drainMeteredStream(t, probe.ch)
	if got := cb.State(); got != circuitbreaker.StateClosed {
		t.Errorf("breaker state after the probe stream succeeded = %v, want closed", got)
	}
}
