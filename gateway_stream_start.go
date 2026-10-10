package aigateway

import (
	"context"
	"errors"
	"fmt"
	"runtime/trace"

	"github.com/ferro-labs/ai-gateway/pkg/circuitbreaker"
	"github.com/ferro-labs/ai-gateway/providers"
)

// startStreamWithStrategy runs the stream START through the same pipeline
// /v1/chat/completions uses, so target ordering, per-target retry, the circuit
// breaker, the concurrency limit and "nothing here can serve this" are the
// gateway's one implementation rather than streaming's own copy of it. It
// returns the target that answered — or, on failure, the last one attempted —
// alongside the raw provider channel.
//
// Only the START is the pipeline's. Everything after it belongs to whatever
// watches the channel drain, which is why the plan sets responseOutlivesCall:
// see targetPlan for what that hands over and why it must be handed over.
//
// If no configured target is viable for the model (wrong model, not a
// StreamProvider, not registered) the request is refused with
// core.ErrNoCapableProvider: targets is an allowlist, so a provider that is
// registered but not listed never serves a stream. A returned channel is never
// replayed.
//
// startCtx bounds this whole selection/retry phase only — it is what the
// pipeline and strategies.WaitBeforeRetry check for expiry. streamCtx is the
// context every CompleteStream call actually runs under — each on a child of
// its own, see raceCompleteStream — and must stay free of that deadline: a
// provider keeps reading its response body on whatever context it
// was called with for as long as the returned channel is alive, so a
// start-phase timeout attached to streamCtx would tear down an
// already-successful stream the moment the clock ran out.
// The third return is what the successful start hands the end of its stream —
// see streamStart. The zero streamStart on failure.
func (g *Gateway) startStreamWithStrategy(startCtx, streamCtx context.Context, req providers.Request) (routedTarget, <-chan providers.StreamChunk, streamStart, error) {
	g.mu.Lock()
	g.ensureCircuitBreakersLocked()
	g.ensureProviderLimitersLocked()
	g.mu.Unlock()

	keys, err := g.streamingTargetOrder(req)
	if err != nil {
		return routedTarget{}, nil, streamStart{}, err
	}

	plan := g.planFor(req.Model, keys)
	plan.responseOutlivesCall = true

	var started streamStart
	raw, target, err := routeTargets(startCtx, g, plan, req, streamCapable, startStreamOn(streamCtx, &started))
	if err != nil {
		return target, nil, streamStart{}, err
	}
	return target, raw, started, nil
}

// streamStart is what a successful stream start hands to whatever observes the
// end of its stream, because both outlive the call that started it.
type streamStart struct {
	// admission is the breaker admission the start holds — taken on the breaker
	// INSTANCE the pipeline actually called, never re-fetched from
	// g.circuitBreakers by name, and tagged with the generation it was admitted
	// in. A ReloadConfig that retires and rebuilds a target's breaker while a
	// stream is mid-start would otherwise hand the stream's eventual outcome to
	// a fresh breaker whose state machine never admitted it. The zero Admission
	// when the target has no breaker configured.
	admission circuitbreaker.Admission
	// release ends the context the start ran on, which the stream keeps running
	// on (see raceCompleteStream). It must be called once the stream has ended:
	// the context is a child of the caller's, and a caller context that outlives
	// many requests would otherwise keep one child per stream it ever served.
	release context.CancelFunc
}

// streamCapable is streaming's candidacy gate: a target whose provider cannot
// stream is not a candidate and is never called.
func streamCapable(p providers.Provider) bool {
	_, ok := providers.As[providers.StreamProvider](p)
	return ok
}

// startStreamOn builds streaming's leaf call.
//
// It is the one surface whose leaf call is a closure rather than a package-level
// function, because the call needs TWO contexts and the pipeline supplies only
// one. The attempt context the pipeline passes bounds the WAIT for the provider
// to answer; streamCtx — captured here — is what the call itself runs on, and
// must outlive the attempt because the channel it returns does.
//
// started receives what each attempt's start hands its stream — the zero
// streamStart when the attempt failed to start. The walk runs attempts
// sequentially and returns on the one that succeeds, so after a successful walk
// it holds the admission and context of the live stream, which are the only
// ones its end may resolve.
func startStreamOn(streamCtx context.Context, started *streamStart) targetCall[providers.Request, <-chan providers.StreamChunk] {
	return func(attemptCtx context.Context, p providers.Provider, req providers.Request, upstreamModel string) (<-chan providers.StreamChunk, error) {
		req.Model = upstreamModel
		*started = streamStart{}
		var (
			raw      <-chan providers.StreamChunk
			attempt  streamStart
			startErr error
		)
		trace.WithRegion(attemptCtx, "gateway.route_stream.provider.start", func() {
			raw, attempt, startErr = startStreamAttempt(attemptCtx, streamCtx, p, req)
		})
		if startErr != nil {
			return nil, startErr
		}
		*started = attempt
		return raw, nil
	}
}

// errNilStream is a provider answering a stream start with neither a channel
// nor an error. The streaming counterpart of errNilProviderResponse.
var errNilStream = errors.New("provider returned a nil stream")

// startStreamAttempt performs one stream start under the breaker and limiter
// the pipeline decorated p with (see undecorate), and owns the start's breaker
// admission until the start succeeds. Every way the start can fail resolves it
// here, exactly once; a start that succeeds hands it back, held, for the end of
// the stream to resolve.
//
// That ownership is what lets a start abandoned on the gateway's own deadline
// count. The wait ends while the call may still be running — or never return
// at all, for an upstream that accepted the connection and went silent — so
// waiting for its result to resolve the breaker would resolve it late or never,
// and a hung streaming upstream stayed in rotation forever. The admission is
// resolved when the wait ends, classified by WHY it ended: a targets[].timeout
// or request_timeout elapsing is the provider being too slow, as it is on the
// unary surfaces; the caller hanging up or its own deadline is not.
//
// The concurrency slot is taken on the WAIT context, before the provider is
// asked, so time spent queued behind the target's own max_concurrency is
// never mistaken for time the provider spent not answering: a wait that ends
// in the queue is a shed (ErrProviderSaturated), which the breaker does not
// count, exactly as on the unary surfaces. It also leaves the queue rather than
// starting, later, a stream nobody will read. Once taken, the slot belongs to
// the start and then to the stream — streamHolding releases it once, whether
// the start fails, the stream ends, or an abandoned start is cancelled.
func startStreamAttempt(waitCtx, streamCtx context.Context, p providers.Provider, req providers.Request) (<-chan providers.StreamChunk, streamStart, error) {
	inner, cb, lim := undecorate(p)
	// streamCapable already proved this of the undecorated provider, so the
	// assertion cannot fail.
	sp, ok := providers.As[providers.StreamProvider](inner)
	if !ok {
		return nil, streamStart{}, fmt.Errorf("provider %s does not support streaming", p.Name())
	}

	var adm circuitbreaker.Admission
	if cb != nil {
		if adm, ok = cb.Admit(); !ok {
			return nil, streamStart{}, circuitbreaker.ErrCircuitOpen
		}
	}
	if lim != nil {
		if err := lim.acquire(waitCtx); err != nil {
			recordCircuitBreakerOutcome(waitCtx, adm, err)
			return nil, streamStart{}, err
		}
	}

	raw, release, abandoned, err := raceCompleteStream(waitCtx, streamCtx, sp, lim, req)
	if err != nil {
		// Classified against the context whose bound produced the error: an
		// abandoned wait against the wait context, a start that answered with
		// its own error against the context it ran on.
		classifyCtx := streamCtx
		if abandoned {
			classifyCtx = waitCtx
		}
		recordCircuitBreakerOutcome(classifyCtx, adm, err)
		return nil, streamStart{}, err
	}
	return raw, streamStart{admission: adm, release: release}, nil
}

// raceCompleteStream bounds only the wait for the stream start to return. The
// call itself runs on a child of streamCtx that belongs to this attempt alone;
// waitCtx is consulted solely to decide how long to keep waiting for a result.
// If waitCtx expires first, the attempt is abandoned and reported as a failure
// so the pipeline can retry or fall back. abandoned reports which of the two
// happened.
//
// A start that wins hands back release with its channel: the stream keeps
// running on the attempt's context, which must therefore outlive the start,
// and release ends it once the stream has. A start that fails has its context
// ended here.
//
// When lim is non-nil the caller has already taken its slot, and the start
// runs through lim.streamHolding, which owns it from then on.
//
// An abandoned attempt has nobody left to answer, so its context is cancelled
// the moment the wait ends, while the call may still be running. A start that
// then answers anyway finds its context already ended: the upstream stops
// generating — and billing — an answer nobody will read, and the concurrency
// slot comes back now rather than when that answer would have finished. Left
// running on streamCtx, it ran to completion for any caller whose context
// outlives the request, holding the target's slot throughout. Whatever it has
// already produced is still drained, so a producer that does not guard its
// sends on ctx can reach its close. Nothing else is touched: the breaker
// admission was resolved when the wait was abandoned, and resolving it again
// from the late result would count one call twice.
func raceCompleteStream(waitCtx, streamCtx context.Context, sp providers.StreamProvider, lim *providerLimiter, req providers.Request) (_ <-chan providers.StreamChunk, release context.CancelFunc, abandoned bool, _ error) {
	type startResult struct {
		ch  <-chan providers.StreamChunk
		err error
	}
	callCtx, cancelCall := context.WithCancel(streamCtx)
	done := make(chan startResult, 1)
	go func() {
		var r startResult
		if lim != nil {
			r.ch, r.err = lim.streamHolding(callCtx, sp, req)
		} else {
			r.ch, r.err = sp.CompleteStream(callCtx, req)
			if r.err == nil && r.ch == nil {
				r.err = errNilStream
			}
		}
		done <- r
	}()
	select {
	case r := <-done:
		if r.err != nil {
			cancelCall()
			return nil, nil, false, r.err
		}
		return r.ch, cancelCall, false, nil
	case <-waitCtx.Done():
		cancelCall()
		go func() {
			r := <-done
			if r.ch != nil {
				for range r.ch { //nolint:revive // drain so the provider's producer can finish
				}
			}
		}()
		return nil, nil, true, context.Cause(waitCtx)
	}
}

// streamingTargetOrder resolves the strategy — the same object Route executes —
// and asks it for the streaming target order, so both paths share one ordering
// implementation. A getStrategy error surfaces identically on both paths; for
// ValidateConfig-passing gateways getStrategy does not error here.
func (g *Gateway) streamingTargetOrder(req providers.Request) ([]string, error) {
	s, err := g.getStrategy()
	if err != nil {
		return nil, err
	}
	return s.SelectTargets(req)
}

// suppressUsageForClient reports whether the caller explicitly declined the
// usage block (stream_options.include_usage: false). It changes only what the
// client is sent: accounting always reads the usage the provider reported.
func suppressUsageForClient(req providers.Request) bool {
	return req.ClientStreamOptions != nil && !req.ClientStreamOptions.IncludeUsage
}

// responseStream replays a complete response — a cache hit, or the answer the
// MCP loop settled on — as a one-chunk stream. These streams never pass through
// streamwrap.Meter, so the client's usage opt-out is applied here: honoured on
// a provider-served stream and ignored on these, it left whether a client that
// declined usage received it to depend on where the answer came from.
func responseStream(resp *providers.Response, suppressUsage bool) <-chan providers.StreamChunk {
	ch := make(chan providers.StreamChunk, 1)
	streamChoices := make([]providers.StreamChoice, len(resp.Choices))
	for i, c := range resp.Choices {
		streamChoices[i] = providers.StreamChoice{
			Index: c.Index,
			Delta: providers.MessageDelta{
				Role:      c.Message.Role,
				Content:   c.Message.Content,
				ToolCalls: c.Message.ToolCalls,
			},
			FinishReason: c.FinishReason,
		}
	}
	chunk := providers.StreamChunk{
		ID:      resp.ID,
		Object:  "chat.completion.chunk",
		Created: resp.Created,
		Model:   resp.Model,
		Choices: streamChoices,
	}
	if !suppressUsage {
		chunk.Usage = &resp.Usage
	}
	ch <- chunk
	close(ch)
	return ch
}

// Target resolution — registered, serves the model, can stream, and decorated
// with its breaker and limiter — is the pipeline's resolveTarget now, gated by
// streamCapable above.
