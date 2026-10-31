package aigateway

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/notify"
	"github.com/ferro-labs/ai-gateway/pkg/circuitbreaker"
	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

// notifierRetireGrace bounds how long a notifier replaced by a config reload
// keeps delivering what it had already queued.
const notifierRetireGrace = 5 * time.Second

// buildNotifier constructs the webhook notifier for cfg, or nil when no
// webhook is configured or none could be built.
func (g *Gateway) buildNotifier(cfg *config.NotificationsConfig) *notify.Notifier {
	return notify.New(cfg, notify.Options{Logger: g.log, ExpandEnv: !g.disableEnvExpansion})
}

// reloadNotifierLocked installs a notifier for the reloaded config when its
// notifications block changed, and retires the old one in the background so
// what it had queued is still delivered. An unchanged block keeps the running
// notifier, and with it each target's error-rate window. Caller holds g.mu.
func (g *Gateway) reloadNotifierLocked(prev, next *config.NotificationsConfig) {
	if reflect.DeepEqual(prev, next) {
		return
	}
	old := g.notifier.Swap(g.buildNotifier(next))
	g.retireNotifier(old)
}

// retireNotifier drains n within notifierRetireGrace without holding up the
// caller. Close waits for every retirement it starts.
func (g *Gateway) retireNotifier(n *notify.Notifier) {
	if n == nil {
		return
	}
	g.pendingNotifierCloses.Add(1)
	go func() {
		defer g.pendingNotifierCloses.Done()
		ctx, cancel := context.WithTimeout(context.Background(), notifierRetireGrace)
		defer cancel()
		n.Close(ctx)
	}()
}

// circuitEvent maps the state a breaker entered to the event that reports it.
func circuitEvent(to circuitbreaker.State) string {
	switch to {
	case circuitbreaker.StateOpen:
		return config.WebhookEventCircuitOpened
	case circuitbreaker.StateHalfOpen:
		return config.WebhookEventCircuitHalfOpen
	case circuitbreaker.StateClosed:
		return config.WebhookEventCircuitClosed
	}
	return ""
}

// circuitTransitionHook returns the state-change hook installed on target's
// breaker. It runs under the breaker's lock, so it reads only lock-free state
// — the current notifier and the published routing snapshot — and never g.mu,
// which a caller reading breaker state may already hold.
func (g *Gateway) circuitTransitionHook(target string) func(from, to circuitbreaker.State) {
	return func(from, to circuitbreaker.State) {
		n := g.notifier.Load()
		event := circuitEvent(to)
		if !n.Wants(event) {
			return
		}
		n.Publish(notify.Event{
			Type:     event,
			Target:   target,
			Provider: g.providerNameOf(target),
			Details:  map[string]any{"from": from.String(), "to": to.String()},
		})
	}
}

// retireCircuitBreakerLocked settles the notifications of a breaker a reload
// is dropping. kept is whether the new config still lists target.
//
// The breaker stops reporting first. Requests it admitted before the reload
// still resolve against it, and a failure among them can trip it; but it no
// longer guards the target, so a transition it makes from here on describes
// nothing the gateway does, and reporting one opened an incident that no
// circuit.closed would ever end.
//
// A target the reload keeps is then served through a fresh, closed breaker or
// through none, which admits as a closed one does. When the breaker it replaces
// was not closed, that change is the target's circuit closing, and it is
// reported as one. A target the reload removes has no circuit left to report.
// Caller holds g.mu.
func (g *Gateway) retireCircuitBreakerLocked(target string, cb *circuitbreaker.CircuitBreaker, kept bool) {
	cb.SetStateChangeHook(nil)
	if !kept {
		return
	}
	from := cb.State()
	if from == circuitbreaker.StateClosed {
		return
	}
	n := g.notifier.Load()
	if !n.Wants(config.WebhookEventCircuitClosed) {
		return
	}
	n.Publish(notify.Event{
		Type:     config.WebhookEventCircuitClosed,
		Target:   target,
		Provider: g.providerNameOf(target),
		Details:  map[string]any{"from": from.String(), "to": circuitbreaker.StateClosed.String()},
	})
}

// providerNameOf returns the canonical provider registered under target, or ""
// when none is. Lock-free: it reads the published routing snapshot.
func (g *Gateway) providerNameOf(target string) string {
	if p := g.routing().providers[target]; p != nil {
		return providers.CanonicalName(p)
	}
	return ""
}

// emitLimitExceeded is the sink installed on plugins that report the first
// refusal of a limit (plugin.LimitReporter). A spend limit becomes
// budget.exceeded. The key is named by its opaque id, never the credential.
func (g *Gateway) emitLimitExceeded(le plugin.LimitExceeded) {
	if le.Kind != plugin.LimitSpend {
		return
	}
	n := g.notifier.Load()
	if !n.Wants(config.WebhookEventBudgetExceeded) {
		return
	}
	n.Publish(notify.Event{
		Type: config.WebhookEventBudgetExceeded,
		Details: map[string]any{
			"plugin":     le.Plugin,
			"store_id":   le.Scope,
			"api_key_id": le.KeyID,
			"limit_usd":  le.Limit,
			"spent_usd":  le.Observed,
		},
	})
}

// observeAttemptOutcome feeds one routing attempt into every error-rate
// webhook.
//
// An attempt counts as an error on the terms it counts toward a circuit
// breaker, and a 429 counts too: the breaker excludes a rate limit because it
// passes, but a sustained run of them is exactly what an error-rate alert is
// for. A failure that is nobody's fault at the target — the caller hanging up,
// the gateway shedding under its own concurrency limit, a parameter refused
// before the call — is not a sample at all, and neither is an open circuit's
// refusal, which never reached the target.
func (g *Gateway) observeAttemptOutcome(ctx context.Context, target routedTarget, err error) {
	n := g.notifier.Load()
	if !n.WantsAttempts() {
		return
	}
	failed := false
	if err != nil {
		if errors.Is(err, circuitbreaker.ErrCircuitOpen) {
			return
		}
		if !shouldRecordCircuitBreakerFailure(ctx, err) && !isRateLimitError(err) {
			return
		}
		failed = true
	}
	n.ObserveAttempt(target.key, target.priceProvider, failed)
}
