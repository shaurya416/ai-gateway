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
	"github.com/ferro-labs/ai-gateway/plugin/budget"
	"github.com/ferro-labs/ai-gateway/providers"
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
