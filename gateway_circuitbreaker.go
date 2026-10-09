package aigateway

import (
	"context"
	"errors"
	"time"

	"github.com/ferro-labs/ai-gateway/pkg/circuitbreaker"
	"github.com/ferro-labs/ai-gateway/pkg/metrics"
	"github.com/ferro-labs/ai-gateway/providers"
)

// cbProvider and its helpers wrap a Provider with a per-provider circuit
// breaker for the gateway's routing paths.

// cbProvider wraps a Provider with a circuit breaker.
//
// It carries no CompleteStream. A stream's breaker outcome is known only when
// the stream ends, long after CompleteStream has returned, and that signature
// has no room to hand the admission to whoever ends it. The streaming pipeline
// takes the breaker off this wrapper instead and resolves it itself — see
// startStreamAttempt.
type cbProvider struct {
	providers.Provider
	cb *circuitbreaker.CircuitBreaker
}

// UnwrapIdentity exposes the provider beneath the breaker. A circuit breaker
// adds behaviour without becoming a vendor, so identity is safe to see through
// while capabilities are not — this type must never implement
// ProviderUnwrapper, or As would resolve CompleteStream/Embed past the breaker
// and lose it. See core.IdentityUnwrapper.
func (p *cbProvider) UnwrapIdentity() providers.Provider { return p.Provider }

func (p *cbProvider) Complete(ctx context.Context, req providers.Request) (resp *providers.Response, err error) {
	adm, ok := p.cb.Admit()
	if !ok {
		return nil, circuitbreaker.ErrCircuitOpen
	}
	// Deferred so a panic from p.Provider.Complete still releases the
	// half-open probe Admit() just admitted. Without this, a panicking probe
	// leaks halfOpenProbes forever: resolveState() only turns Open into
	// HalfOpen on a timeout, it never repairs a HalfOpen circuit stuck at its
	// probe cap, so Admit() would reject every request for this provider
	// until the process restarts. A panic is treated as a failure, then
	// re-raised so it still propagates to the caller.
	defer func() {
		if r := recover(); r != nil {
			adm.Failure()
			panic(r)
		}
		recordCircuitBreakerOutcome(ctx, adm, err)
	}()
	resp, err = p.Provider.Complete(ctx, req)
	return resp, err
}

// shouldRecordCircuitBreakerFailure reports whether an error should count toward
// opening the circuit.
//
// The distinction that matters is WHOSE fault the failure is — the same question
// the error_type metric label answers, which is why both are decided by the one
// classifier (metrics.ProviderErrorType) rather than by two switches that drift:
//
//   - The gateway's own request deadline (Config.RequestTimeout) firing, and the
//     streaming idle bound (streamio.ErrIdleTimeout), both mean the provider was
//     too slow to answer. Those classify as ErrTypeTimeout, are the provider's
//     fault, and MUST trip the breaker. Treating either as caller cancellation
//     would leave a hung provider in rotation forever while /readyz — whose only
//     provider signal is circuit state — kept reporting the pod ready.
//   - A caller-side cancellation or a caller-supplied deadline classifies as
//     ErrTypeClientCanceled and is excluded, so transient client behavior cannot
//     block healthy traffic.
//   - Shedding under our own per-target concurrency limit classifies as
//     ErrTypeBackpressure: a 429 the provider never saw.
//   - A rejection the gateway raised itself before ever reaching the provider (an
//     unsupported parameter under compatibility.on_unsupported_param=reject) is a
//     client error that never touched the network, and must never blame the
//     provider. It has no error_type of its own, so it is named here.
//   - Rate limits are expected and temporary, and stay excluded.
func shouldRecordCircuitBreakerFailure(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}

	var unsupportedParam *providers.UnsupportedParamError
	if errors.As(err, &unsupportedParam) {
		return false
	}

	switch metrics.ProviderErrorType(ctx, err) {
	case metrics.ErrTypeBackpressure, metrics.ErrTypeClientCanceled:
		return false
	}
	return !isRateLimitError(err)
}

// recordCircuitBreakerOutcome resolves one admitted upstream call: a
// blameworthy failure trips the breaker, a failure that is not the provider's
// fault releases the half-open probe instead, and a success closes it. Used by
// the pipeline, by the stream path — at the start for a start that failed, and
// once the stream finishes for one that began — and by withTargetBreaker.
//
// The outcome lands on the generation that admitted the call and no other, so
// a call that outlives a state transition — admitted while Closed, finishing
// after the circuit opened and aged into HalfOpen — cannot close the circuit or
// free a probe slot it never took. See circuitbreaker.Admission.
//
// It records the outcome and nothing else. The gateway_circuit_breaker_state
// gauge is not written here — it is resolved from the live breakers on each
// scrape (see Gateway.CircuitBreakerStates), because a breaker also changes
// state on a timer that no request outcome observes.
func recordCircuitBreakerOutcome(ctx context.Context, adm circuitbreaker.Admission, err error) {
	if err != nil {
		if !shouldRecordCircuitBreakerFailure(ctx, err) {
			adm.Release()
			return
		}
		adm.Failure()
		return
	}
	adm.Success()
}

// withTargetBreaker runs fn under the target's circuit breaker, for the surfaces
// cbProvider cannot wrap. Embedding and image providers are reached through
// optional interfaces (EmbeddingProvider / ImageProvider), and a wrapper
// embedding providers.Provider would fail those type assertions and break the
// surface outright — the same constraint that puts the concurrency limiter at
// the call site. A target with no breaker configured runs fn unchanged.
//
// Composition mirrors decorateProvider: the breaker is OUTERMOST and the
// limiter INNERMOST, so an open circuit fails fast without ever taking an
// in-flight slot or a queue position.
func (g *Gateway) withTargetBreaker(ctx context.Context, target string, fn func(context.Context) error) error {
	g.mu.RLock()
	cb := g.circuitBreakers[target]
	g.mu.RUnlock()

	if cb == nil {
		return fn(ctx)
	}
	adm, ok := cb.Admit()
	if !ok {
		return circuitbreaker.ErrCircuitOpen
	}
	// Deferred for the same reason as cbProvider.Complete: fn panicking must
	// still resolve the half-open probe Admit() admitted, or the breaker gets
	// stuck rejecting this target forever with no self-healing. A panic
	// counts as a failure and is re-raised afterward, never swallowed.
	defer func() {
		if r := recover(); r != nil {
			adm.Failure()
			panic(r)
		}
	}()
	err := fn(ctx)
	recordCircuitBreakerOutcome(ctx, adm, err)
	return err
}

// ensureCircuitBreakersLocked creates circuit breakers for configured targets.
// Caller must hold g.mu.
func (g *Gateway) ensureCircuitBreakersLocked() {
	for _, t := range g.config.Targets {
		if t.CircuitBreaker == nil {
			continue
		}
		if _, exists := g.circuitBreakers[t.VirtualKey]; exists {
			continue
		}
		// circuitbreaker.New reads an unparseable duration as zero and applies its
		// 30s default, so without this the target reopens on a schedule nobody
		// configured and nothing ever says why.
		timeout, err := time.ParseDuration(t.CircuitBreaker.Timeout)
		if err != nil && t.CircuitBreaker.Timeout != "" {
			g.log.Warn("target circuit_breaker.timeout is not a duration; applying the default",
				"target", t.VirtualKey, "timeout", t.CircuitBreaker.Timeout)
		}
		g.circuitBreakers[t.VirtualKey] = circuitbreaker.New(
			t.CircuitBreaker.FailureThreshold,
			t.CircuitBreaker.SuccessThreshold,
			t.CircuitBreaker.MaxHalfThreshold,
			timeout,
		)
	}
}

// isRateLimitError checks if the error is a 429 rate limit response.
// Rate limits are expected and temporary — they should not trip the circuit breaker.
func isRateLimitError(err error) bool {
	return providers.ParseStatusCode(err) == 429
}
