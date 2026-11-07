package aigateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/authctx"
	"github.com/ferro-labs/ai-gateway/internal/notify"
	"github.com/ferro-labs/ai-gateway/pkg/metrics"
	"github.com/ferro-labs/ai-gateway/plugin/budget"
	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/providers/core"
	dto "github.com/prometheus/client_model/go"
)

// hookReceiver is a webhook endpoint recording every payload it is sent.
type hookReceiver struct {
	*httptest.Server
	mu     sync.Mutex
	bodies [][]byte
}

func newHookReceiver(t *testing.T) *hookReceiver {
	t.Helper()
	r := &hookReceiver{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.bodies = append(r.bodies, body)
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *hookReceiver) payloads(t *testing.T) []notify.Payload {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]notify.Payload, 0, len(r.bodies))
	for _, b := range r.bodies {
		var p notify.Payload
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatalf("payload is not JSON: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func (r *hookReceiver) raw() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, body := range r.bodies {
		b.Write(body)
	}
	return b.String()
}

func (r *hookReceiver) waitForCount(t *testing.T, n int) []notify.Payload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := r.payloads(t)
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("receiver got %d payloads, want %d", len(got), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settle gives a stray delivery time to arrive before a test asserts that
// nothing more did.
func settle() { time.Sleep(50 * time.Millisecond) }

func eventsOf(ps []notify.Payload) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Event
	}
	return out
}

const notifyPrompt = "the-prompt-text-must-never-leave"

func notifyRequest() providers.Request {
	return providers.Request{Model: "gpt-4o", Messages: []providers.Message{{Role: "user", Content: notifyPrompt}}}
}

// TestNotifications_CircuitTransitionsFireOncePerTransition walks a target's
// breaker closed → open → half-open → closed through real routing, with many
// more requests than transitions, and requires one event per transition.
func TestNotifications_CircuitTransitionsFireOncePerTransition(t *testing.T) {
	rcv := newHookReceiver(t)
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets: []config.Target{{
			VirtualKey:     mockProviderName,
			CircuitBreaker: &config.CircuitBreakerConfig{FailureThreshold: 2, Timeout: "1m"},
		}},
		Notifications: &config.NotificationsConfig{Webhooks: []config.WebhookConfig{{
			Name:   "circuit",
			URL:    rcv.URL,
			Events: []string{config.WebhookEventCircuitOpened, config.WebhookEventCircuitHalfOpen, config.WebhookEventCircuitClosed},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var failing atomic.Bool
	gw.RegisterProvider(&mockProvider{
		name:   mockProviderName,
		models: []string{"gpt-4o"},
		completeFn: func(context.Context, providers.Request) (*providers.Response, error) {
			if failing.Load() {
				return nil, errors.New("upstream exploded")
			}
			return &providers.Response{Model: "gpt-4o", Choices: []providers.Choice{{Message: providers.Message{Role: "assistant", Content: "ok"}}}}, nil
		},
	})
	cb := gw.circuitBreakers[mockProviderName]
	now := time.Now()
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	cb.SetNowForTest(func() time.Time { return time.Unix(0, clock.Load()) })

	ctx := context.Background()
	failing.Store(true)
	for range 6 { // two failures open it; the rest are refused by the open circuit
		_, _ = gw.Route(ctx, notifyRequest())
	}
	_ = gw.Readiness() // reading state while open reports nothing new
	_ = gw.CircuitBreakerStates()

	clock.Add(int64(2 * time.Minute))
	failing.Store(false)
	for range 4 { // the first is the half-open probe that closes it
		if _, err := gw.Route(ctx, notifyRequest()); err != nil {
			t.Fatalf("recovered request: %v", err)
		}
	}

	rcv.waitForCount(t, 3)
	settle()
	got := rcv.payloads(t)
	want := []string{config.WebhookEventCircuitOpened, config.WebhookEventCircuitHalfOpen, config.WebhookEventCircuitClosed}
	if strings.Join(eventsOf(got), ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", eventsOf(got), want)
	}
	for _, p := range got {
		if p.Target != mockProviderName || p.Provider != mockProviderName {
			t.Fatalf("payload names target %q provider %q", p.Target, p.Provider)
		}
	}
	if got[0].Details["from"] != "closed" || got[0].Details["to"] != "open" {
		t.Fatalf("details = %v", got[0].Details)
	}
	if strings.Contains(rcv.raw(), notifyPrompt) {
		t.Fatal("a payload carried prompt text")
	}
}

// TestNotifications_BudgetExceededFiresOncePerRun drives the real budget
// plugin past its cap with one key and keeps retrying; one event results, and
// it names the key by its id without any request content.
func TestNotifications_BudgetExceededFiresOncePerRun(t *testing.T) {
	rcv := newHookReceiver(t)
	budgetCfg := map[string]any{
		"store_id":            "notify-budget",
		"spend_limit_usd":     0.001,
		"input_per_m_tokens":  3.0,
		"output_per_m_tokens": 15.0,
	}
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: mockProviderName}},
		Plugins: []config.PluginConfig{
			{Name: "budget", Type: "ratelimit", Stage: "before_request", Enabled: true, Config: budgetCfg},
			{Name: "budget", Type: "ratelimit", Stage: "after_request", Enabled: true, Config: budgetCfg},
		},
		Notifications: &config.NotificationsConfig{Webhooks: []config.WebhookConfig{{
			Name:   "budget",
			URL:    rcv.URL,
			Events: []string{config.WebhookEventBudgetExceeded},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	gw.RegisterProvider(&mockProvider{
		name:   mockProviderName,
		models: []string{"gpt-4o"},
		resp: &providers.Response{
			Model:   "gpt-4o",
			Choices: []providers.Choice{{Message: providers.Message{Role: "assistant", Content: "ok"}}},
			Usage:   providers.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}, // $0.00105
		},
	})
	if err := gw.LoadPlugins(); err != nil {
		t.Fatal(err)
	}
	budget.ResetStore("notify-budget")

	ctx := authctx.WithKeyID(context.Background(), "key-123")
	if _, err := gw.Route(ctx, notifyRequest()); err != nil {
		t.Fatalf("first request is under the cap: %v", err)
	}
	refused := 0
	for range 5 {
		if _, err := gw.Route(ctx, notifyRequest()); err != nil {
			refused++
		}
	}
	if refused != 5 {
		t.Fatalf("refused %d of 5 over-cap requests", refused)
	}

	rcv.waitForCount(t, 1)
	settle()
	got := rcv.payloads(t)
	if len(got) != 1 {
		t.Fatalf("five refusals produced %d events, want 1", len(got))
	}
	p := got[0]
	if p.Event != config.WebhookEventBudgetExceeded || p.Details["api_key_id"] != "key-123" ||
		p.Details["store_id"] != "notify-budget" || p.Details["limit_usd"] != 0.001 {
		t.Fatalf("payload = %+v", p)
	}
	if strings.Contains(rcv.raw(), notifyPrompt) {
		t.Fatal("a payload carried prompt text")
	}
}

// TestNotifications_ErrorRateHighFiresOnce feeds routing outcomes into the
// error-rate tracker and requires one event for a sustained outage.
func TestNotifications_ErrorRateHighFiresOnce(t *testing.T) {
	rcv := newHookReceiver(t)
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: mockProviderName}},
		Notifications: &config.NotificationsConfig{Webhooks: []config.WebhookConfig{{
			Name:      "errors",
			URL:       rcv.URL,
			Events:    []string{config.WebhookEventErrorRateHigh},
			ErrorRate: &config.ErrorRateConfig{Threshold: 0.5, Window: "1m", MinRequests: 4},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var failing atomic.Bool
	gw.RegisterProvider(&mockProvider{
		name:   mockProviderName,
		models: []string{"gpt-4o"},
		completeFn: func(context.Context, providers.Request) (*providers.Response, error) {
			if failing.Load() {
				return nil, errors.New("upstream exploded")
			}
			return &providers.Response{Model: "gpt-4o", Choices: []providers.Choice{{Message: providers.Message{Role: "assistant", Content: "ok"}}}}, nil
		},
	})

	ctx := context.Background()
	for range 4 {
		_, _ = gw.Route(ctx, notifyRequest())
	}
	// A caller that hangs up is not evidence about the target.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for range 10 {
		_, _ = gw.Route(canceled, notifyRequest())
	}
	failing.Store(true)
	for range 20 {
		_, _ = gw.Route(ctx, notifyRequest())
	}

	rcv.waitForCount(t, 1)
	settle()
	got := rcv.payloads(t)
	if len(got) != 1 {
		t.Fatalf("a sustained outage produced %d events, want 1", len(got))
	}
	p := got[0]
	if p.Event != config.WebhookEventErrorRateHigh || p.Target != mockProviderName {
		t.Fatalf("payload = %+v", p)
	}
	// Four successes then four failures is the first window at 50%.
	if p.Details["requests"] != float64(8) || p.Details["errors"] != float64(4) {
		t.Fatalf("details = %v, want the window that crossed: 4 of 8", p.Details)
	}
}

// TestNotifications_ErrorRateCountsPassthroughForwards: POST /v1/responses and
// the /v1/* pass-through reach a target under its circuit breaker, which
// scores each forward, so each forward is an error-rate sample on the same
// terms: a 5xx is a failure, a 429 is a failure, and any other 4xx is the
// caller's own request answered, which is a success.
func TestNotifications_ErrorRateCountsPassthroughForwards(t *testing.T) {
	rcv := newHookReceiver(t)
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: mockProviderName}},
		Notifications: &config.NotificationsConfig{Webhooks: []config.WebhookConfig{{
			Name:      "errors",
			URL:       rcv.URL,
			Events:    []string{config.WebhookEventErrorRateHigh},
			ErrorRate: &config.ErrorRateConfig{Threshold: 0.5, Window: "1m", MinRequests: 8},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	gw.RegisterProvider(&mockProvider{name: mockProviderName, models: []string{"gpt-4o"}})

	status := func(code int) func(context.Context) error {
		return func(context.Context) error {
			return &core.HTTPStatusError{StatusCode: code, Message: "upstream answer"}
		}
	}
	ctx := context.Background()
	// Four answered requests, a 400 among them: below the threshold whatever
	// follows until the eighth sample.
	for range 3 {
		_ = gw.RouteResponses(ctx, mockProviderName, "gpt-4o", "hi", true, 0, nil, func(context.Context) error { return nil })
	}
	_ = gw.RoutePassthrough(ctx, mockProviderName, "gpt-4o", "hi", true, status(http.StatusBadRequest))
	_ = gw.RouteResponses(ctx, mockProviderName, "gpt-4o", "hi", true, 0, nil, status(http.StatusServiceUnavailable))
	_ = gw.RoutePassthrough(ctx, mockProviderName, "gpt-4o", "hi", true, status(http.StatusTooManyRequests))
	settle()
	if got := rcv.payloads(t); len(got) != 0 {
		t.Fatalf("fired before the window held min_requests samples: %v", eventsOf(got))
	}
	for range 2 {
		_ = gw.RouteResponses(ctx, mockProviderName, "gpt-4o", "hi", true, 0, nil, status(http.StatusBadGateway))
	}

	got := rcv.waitForCount(t, 1)
	settle()
	if len(rcv.payloads(t)) != 1 {
		t.Fatalf("got %d events, want 1", len(rcv.payloads(t)))
	}
	p := got[0]
	if p.Event != config.WebhookEventErrorRateHigh || p.Target != mockProviderName || p.Provider != mockProviderName {
		t.Fatalf("payload = %+v", p)
	}
	if p.Details["requests"] != float64(8) || p.Details["errors"] != float64(4) {
		t.Fatalf("details = %v, want 4 failures (503, 429, 502, 502) of 8 forwards", p.Details)
	}
}

// TestNotifications_ReloadKeepsOrReplacesTheNotifier: an unchanged block keeps
// the running notifier, and its error-rate state, across a reload; a changed
// block delivers to the new receiver.
func TestNotifications_ReloadKeepsOrReplacesTheNotifier(t *testing.T) {
	first, second := newHookReceiver(t), newHookReceiver(t)
	cfg := config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: mockProviderName, CircuitBreaker: &config.CircuitBreakerConfig{FailureThreshold: 1, Timeout: "1h"}}},
		Notifications: &config.NotificationsConfig{Webhooks: []config.WebhookConfig{{
			Name: "a", URL: first.URL, Events: []string{config.WebhookEventCircuitOpened},
		}}},
	}
	gw, err := newTestGateway(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	gw.RegisterProvider(&mockProvider{name: mockProviderName, models: []string{"gpt-4o"}, err: errors.New("down")})

	running := gw.notifier.Load()
	same := cfg
	same.Notifications = &config.NotificationsConfig{Webhooks: append([]config.WebhookConfig(nil), cfg.Notifications.Webhooks...)}
	if err := gw.ReloadConfig(context.Background(), same); err != nil {
		t.Fatal(err)
	}
	if gw.notifier.Load() != running {
		t.Fatal("an unchanged notifications block replaced the notifier")
	}

	moved := cfg
	moved.Notifications = &config.NotificationsConfig{Webhooks: []config.WebhookConfig{{
		Name: "a", URL: second.URL, Events: []string{config.WebhookEventCircuitOpened},
	}}}
	if err := gw.ReloadConfig(context.Background(), moved); err != nil {
		t.Fatal(err)
	}
	_, _ = gw.Route(context.Background(), notifyRequest())
	second.waitForCount(t, 1)
	settle()
	if n := len(first.payloads(t)); n != 0 {
		t.Fatalf("the replaced receiver got %d events", n)
	}

	removed := cfg
	removed.Notifications = nil
	if err := gw.ReloadConfig(context.Background(), removed); err != nil {
		t.Fatal(err)
	}
	if gw.notifier.Load() != nil {
		t.Fatal("removing the block left a notifier running")
	}
}

// circuitNotifyConfig is a single-target config whose breaker opens on one
// failure, with every circuit event delivered to url.
func circuitNotifyConfig(url, timeout string) config.Config {
	return config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets: []config.Target{{
			VirtualKey:     mockProviderName,
			CircuitBreaker: &config.CircuitBreakerConfig{FailureThreshold: 1, Timeout: timeout},
		}},
		Notifications: &config.NotificationsConfig{Webhooks: []config.WebhookConfig{{
			Name:   "circuit",
			URL:    url,
			Events: []string{config.WebhookEventCircuitOpened, config.WebhookEventCircuitHalfOpen, config.WebhookEventCircuitClosed},
		}}},
	}
}

// TestNotifications_RetiredBreakerReportsNothing: a reload that changes a
// target's circuit_breaker block replaces its breaker. A request admitted by
// the old breaker and failing after the reload trips that old breaker, which
// no longer guards the target — so it must not report the target's circuit as
// opened while /health, /readyz and the gauge all read it closed, an alert no
// circuit.closed would ever follow.
func TestNotifications_RetiredBreakerReportsNothing(t *testing.T) {
	rcv := newHookReceiver(t)
	cfg := circuitNotifyConfig(rcv.URL, "1h")
	gw, err := newTestGateway(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	gw.RegisterProvider(&mockProvider{
		name:   mockProviderName,
		models: []string{"gpt-4o"},
		completeFn: func(context.Context, providers.Request) (*providers.Response, error) {
			close(entered)
			<-release
			return nil, errors.New("upstream exploded")
		},
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = gw.Route(context.Background(), notifyRequest())
	}()
	<-entered
	if err := gw.ReloadConfig(context.Background(), circuitNotifyConfig(rcv.URL, "2h")); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done

	settle()
	if got := rcv.payloads(t); len(got) != 0 {
		t.Fatalf("a breaker the reload retired reported %v for a target whose circuit is %+v",
			eventsOf(got), gw.Readiness().Providers)
	}
}

// TestNotifications_ReloadClosingAnOpenCircuitReportsIt: a reload that
// replaces an open breaker leaves the target admitting traffic through a closed
// one. The receiver was told the circuit opened, so it is told it closed;
// otherwise the incident stays open on the receiver for as long as the target
// keeps serving. A target the reload removes has no circuit left to report.
func TestNotifications_ReloadClosingAnOpenCircuitReportsIt(t *testing.T) {
	rcv := newHookReceiver(t)
	gw, err := newTestGateway(t, circuitNotifyConfig(rcv.URL, "1h"))
	if err != nil {
		t.Fatal(err)
	}
	gw.RegisterProvider(&mockProvider{name: mockProviderName, models: []string{"gpt-4o"}, err: errors.New("down")})

	_, _ = gw.Route(context.Background(), notifyRequest())
	rcv.waitForCount(t, 1)
	if err := gw.ReloadConfig(context.Background(), circuitNotifyConfig(rcv.URL, "2h")); err != nil {
		t.Fatal(err)
	}
	got := rcv.waitForCount(t, 2)
	want := []string{config.WebhookEventCircuitOpened, config.WebhookEventCircuitClosed}
	if strings.Join(eventsOf(got), ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", eventsOf(got), want)
	}
	if p := got[1]; p.Target != mockProviderName || p.Details["from"] != "open" || p.Details["to"] != "closed" {
		t.Fatalf("closing payload = %+v", p)
	}
	if circuit := gw.Readiness().Providers[0].Circuit; circuit != "closed" {
		t.Fatalf("the reported close disagrees with readiness: circuit %q", circuit)
	}

	// Open it again, then drop the target: nothing is left to report on.
	_, _ = gw.Route(context.Background(), notifyRequest())
	rcv.waitForCount(t, 3)
	removed := circuitNotifyConfig(rcv.URL, "2h")
	removed.Targets = []config.Target{{VirtualKey: "other"}}
	if err := gw.ReloadConfig(context.Background(), removed); err != nil {
		t.Fatal(err)
	}
	settle()
	if got := rcv.payloads(t); len(got) != 3 {
		t.Fatalf("removing the target reported %v", eventsOf(got[3:]))
	}
}

// TestNotifications_CloseDeliversWhatIsQueued: an event published just before
// shutdown still reaches its receiver.
func TestNotifications_CloseDeliversWhatIsQueued(t *testing.T) {
	rcv := newHookReceiver(t)
	gw, err := New(config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: mockProviderName}},
		Notifications: &config.NotificationsConfig{Webhooks: []config.WebhookConfig{{
			Name: "drain", URL: rcv.URL, Events: []string{config.WebhookEventCircuitOpened},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	gw.notifier.Load().Publish(notify.Event{Type: config.WebhookEventCircuitOpened, Target: "x"})
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if n := len(rcv.payloads(t)); n != 1 {
		t.Fatalf("delivered %d of 1 queued events by the time Close returned", n)
	}
}

// TestNotifications_CloseWaitsForTheWorkersItAbandons: against receivers that
// never answer, Close cannot deliver what is queued; at its deadline it
// abandons the rest, and it returns only once every delivery worker has
// exited — so by then every event is accounted for in
// gateway_webhook_deliveries_total and no delivery is still running.
func TestNotifications_CloseWaitsForTheWorkersItAbandons(t *testing.T) {
	hung := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		// Read to the end, so the server notices the client hanging up.
		_, _ = io.Copy(io.Discard, req.Body)
		<-req.Context().Done()
	}))
	defer hung.Close()

	const webhooks, events = 4, 256
	hooks := make([]config.WebhookConfig, 0, webhooks)
	for i := range webhooks {
		hooks = append(hooks, config.WebhookConfig{
			Name:    "close-unwind-" + string(rune('a'+i)),
			URL:     hung.URL,
			Events:  []string{config.WebhookEventCircuitOpened},
			Timeout: "1m",
		})
	}
	gw, err := New(config.Config{
		Strategy:      config.StrategyConfig{Mode: config.ModeSingle},
		Targets:       []config.Target{{VirtualKey: mockProviderName}},
		Notifications: &config.NotificationsConfig{Webhooks: hooks},
	})
	if err != nil {
		t.Fatal(err)
	}
	settled := func(name string) float64 {
		var sum float64
		for _, outcome := range []string{notify.OutcomeDelivered, notify.OutcomeRejected, notify.OutcomeFailed, notify.OutcomeDropped, notify.OutcomeAbandoned} {
			sum += webhookDeliveries(t, name, config.WebhookEventCircuitOpened, outcome)
		}
		return sum
	}
	before := make(map[string]float64, len(hooks))
	for _, h := range hooks {
		before[h.Name] = settled(h.Name)
	}
	n := gw.notifier.Load()
	for range events {
		n.Publish(notify.Event{Type: config.WebhookEventCircuitOpened, Target: "x"})
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	for _, h := range hooks {
		if got := settled(h.Name) - before[h.Name]; got != events {
			t.Fatalf("webhook %s: %v of %d events accounted for when Close returned; its worker was still abandoning the rest", h.Name, got, events)
		}
	}
}

func webhookDeliveries(t *testing.T, name, event, outcome string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := metrics.WebhookDeliveriesTotal.WithLabelValues(name, event, outcome).Write(m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}
