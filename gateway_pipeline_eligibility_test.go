package aigateway

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/observability"
	"github.com/ferro-labs/ai-gateway/pkg/circuitbreaker"
	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// A request that fails on every target reports the last target attempted and
// every attempt made, on the attribution a handler turns into the X-Gateway-*
// headers and on the request span's ferro.routing.attempt.
func TestPipeline_FailureAttributionCountsEveryAttempt(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeFallback},
		Targets:  []config.Target{{VirtualKey: "first"}, {VirtualKey: "second", ModelMap: map[string]string{pipelineModel: "second-upstream"}}},
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	fp := &fakeProvider{}
	gw.SetObservability(fp)
	down := func() (*providers.Response, error) {
		return nil, core.StatusError("upstream", http.StatusServiceUnavailable, "down")
	}
	gw.RegisterProviderAs("first", newCountingProvider("vendor-a", down))
	second := newCountingProvider("vendor-b", down)
	second.models = []string{"second-upstream"}
	gw.RegisterProviderAs("second", second)

	attribution := &RoutingAttribution{}
	ctx := WithRoutingAttribution(context.Background(), attribution)
	if _, err := gw.Route(ctx, pipelineRequest()); err == nil {
		t.Fatal("Route succeeded with every target down")
	}

	want := RoutingAttribution{Provider: "vendor-b", Target: "second", Model: "second-upstream", Attempts: 2}
	if *attribution != want {
		t.Fatalf("attribution = %+v, want %+v", *attribution, want)
	}
	sp := fp.rootSpan()
	if sp == nil {
		t.Fatal("expected a root span")
	}
	if got := sp.attrs[observability.AttrFerroRoutingAttempt]; got != 2 {
		t.Fatalf("ferro.routing.attempt = %v, want 2", got)
	}
}

// An open circuit is skipped during selection, not attempted and refused: the
// request that a healthy sibling serves makes one routing attempt.
func TestPipeline_OpenCircuitIsSkippedWithoutAnAttempt(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeFallback},
		Targets:  []config.Target{openableTarget("tripped"), {VirtualKey: "healthy"}},
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	ok := func() (*providers.Response, error) { return &providers.Response{ID: "ok"}, nil }
	gw.RegisterProviderAs("tripped", newCountingProvider("vendor-a", ok))
	gw.RegisterProviderAs("healthy", newCountingProvider("vendor-b", ok))
	tripBreaker(t, gw, "tripped")

	attribution := &RoutingAttribution{}
	ctx := WithRoutingAttribution(context.Background(), attribution)
	resp, err := gw.Route(ctx, pipelineRequest())
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if resp.Provider != "healthy" {
		t.Fatalf("served by %q, want healthy", resp.Provider)
	}
	if attribution.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1: the open target was attempted rather than skipped", attribution.Attempts)
	}
}

// A HALF-OPEN target stays eligible. Excluding it while a sibling is healthy
// would starve it of the probe that is its only way back to closed.
func TestPipeline_HalfOpenTargetStaysEligibleAndEarnsItsProbe(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeFallback},
		Targets:  []config.Target{openableTarget("recovering"), {VirtualKey: "healthy"}},
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	ok := func() (*providers.Response, error) { return &providers.Response{ID: "ok"}, nil }
	recovering := newCountingProvider("vendor-a", ok)
	healthy := newCountingProvider("vendor-b", ok)
	gw.RegisterProviderAs("recovering", recovering)
	gw.RegisterProviderAs("healthy", healthy)
	tripBreaker(t, gw, "recovering")

	gw.mu.RLock()
	cb := gw.circuitBreakers["recovering"]
	gw.mu.RUnlock()
	later := time.Now().Add(2 * time.Hour) // past openableTarget's 1h timeout
	cb.SetNowForTest(func() time.Time { return later })
	if got := cb.State(); got != circuitbreaker.StateHalfOpen {
		t.Fatalf("breaker = %v, want HalfOpen", got)
	}

	resp, err := gw.Route(context.Background(), pipelineRequest())
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if resp.Provider != "recovering" || recovering.calls.Load() != 1 || healthy.calls.Load() != 0 {
		t.Fatalf("served by %q (recovering calls %d, healthy calls %d), want the half-open target to take the probe",
			resp.Provider, recovering.calls.Load(), healthy.calls.Load())
	}
	if got := cb.State(); got != circuitbreaker.StateClosed {
		t.Fatalf("breaker = %v after a successful probe, want Closed", got)
	}
}

// A content-based rule's target_keys chain is walked on a failover-safe
// failure exactly as a conditional rule's is, and never past its end.
func TestChain_ContentBasedRuleWalksItsChain(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{
			Mode:              config.ModeContentBased,
			ContentConditions: []config.ContentCondition{{Type: config.ContentConditionPromptContains, Value: "hi", TargetKeys: []string{"a", "b"}}},
		},
		Targets: []config.Target{{VirtualKey: "a"}, {VirtualKey: "b"}, {VirtualKey: "c"}},
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	a := newCountingProvider("a", failing(http.StatusServiceUnavailable))
	b := newCountingProvider("b", serving("served-by-b"))
	c := newCountingProvider("c", serving("served-by-c"))
	gw.RegisterProvider(a)
	gw.RegisterProvider(b)
	gw.RegisterProvider(c)

	resp, err := gw.Route(context.Background(), pipelineRequest())
	if err != nil || resp.ID != "served-by-b" {
		t.Fatalf("resp=%v err=%v, want b after a failed", resp, err)
	}
	if a.calls.Load() != 1 || c.calls.Load() != 0 {
		t.Fatalf("a=%d c=%d calls, want a asked once and c, outside the chain, never", a.calls.Load(), c.calls.Load())
	}
}

// The pipeline's own call site resolves a panicking half-open probe as a
// failure, as withTargetBreaker does: a probe left unresolved holds the only
// slot and the target is refused for the life of the process.
func TestPipeline_PanicDoesNotStrandHalfOpenProbe(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets: []config.Target{{
			VirtualKey:     "panicky",
			CircuitBreaker: &config.CircuitBreakerConfig{FailureThreshold: 1, SuccessThreshold: 1, MaxHalfThreshold: 1, Timeout: "1ms"},
		}},
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	var mode atomic.Int32 // 0 fail, 1 panic, 2 serve
	p := newCountingProvider("panicky", func() (*providers.Response, error) {
		switch mode.Load() {
		case 0:
			return nil, core.StatusError("panicky", http.StatusServiceUnavailable, "down")
		case 1:
			panic("provider blew up mid-probe")
		default:
			return &providers.Response{ID: "ok"}, nil
		}
	})
	gw.RegisterProvider(p)
	gw.mu.Lock()
	gw.ensureCircuitBreakersLocked()
	cb := gw.circuitBreakers["panicky"]
	gw.mu.Unlock()
	now := time.Unix(0, 0)
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	cb.SetNowForTest(func() time.Time { return time.Unix(0, clock.Load()) })

	if _, err := gw.Route(context.Background(), pipelineRequest()); err == nil {
		t.Fatal("first call should fail and open the circuit")
	}
	if got := cb.State(); got != circuitbreaker.StateOpen {
		t.Fatalf("breaker = %v, want Open", got)
	}

	clock.Add(int64(5 * time.Millisecond))
	mode.Store(1)
	func() {
		defer func() { _ = recover() }()
		_, _ = gw.Route(context.Background(), pipelineRequest())
	}()

	clock.Add(int64(5 * time.Millisecond))
	mode.Store(2)
	if _, err := gw.Route(context.Background(), pipelineRequest()); err != nil {
		t.Fatalf("post-panic probe: %v; the panicking probe was never resolved", err)
	}
}

// Dropping an open circuit removes that target and no other: the healthy
// targets ahead of it keep their place, so the preferred target still serves.
func TestPipeline_OpenCircuitFilterKeepsTheTargetsAheadOfIt(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeFallback},
		Targets:  []config.Target{{VirtualKey: "first"}, openableTarget("tripped"), {VirtualKey: "last"}},
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	ok := func() (*providers.Response, error) { return &providers.Response{ID: "ok"}, nil }
	first := newCountingProvider("first", ok)
	last := newCountingProvider("last", ok)
	gw.RegisterProvider(first)
	gw.RegisterProvider(newCountingProvider("tripped", ok))
	gw.RegisterProvider(last)
	tripBreaker(t, gw, "tripped")

	resp, err := gw.Route(context.Background(), pipelineRequest())
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if resp.Provider != "first" || last.calls.Load() != 0 {
		t.Fatalf("served by %q (last called %d times), want first: an open circuit behind it reordered the walk",
			resp.Provider, last.calls.Load())
	}
}

// A pass-through body over the gateway's own size limit is the caller's
// failure to deliver, not evidence the upstream is unhealthy.
func TestShouldRecordCircuitBreakerFailure_OversizedBodyNeverBlamesProvider(t *testing.T) {
	err := fmt.Errorf("forward request body: %w", &http.MaxBytesError{Limit: 1 << 20})
	if shouldRecordCircuitBreakerFailure(context.Background(), err) {
		t.Error("a body over the gateway's size limit tripped the target's circuit")
	}
}
