package notify

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/pkg/metrics"
)

// delivery is one request a test receiver saw.
type delivery struct {
	header http.Header
	body   []byte
}

// receiver is an httptest webhook endpoint answering with the statuses it is
// given, in order, and 200 once they run out.
type receiver struct {
	*httptest.Server
	mu       sync.Mutex
	got      []delivery
	statuses []int
	block    chan struct{} // when non-nil, every request waits on it
}

func newReceiver(t *testing.T, statuses ...int) *receiver {
	t.Helper()
	r := &receiver{statuses: statuses}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.got = append(r.got, delivery{header: req.Header.Clone(), body: body})
		status := http.StatusOK
		if len(r.statuses) > 0 {
			status, r.statuses = r.statuses[0], r.statuses[1:]
		}
		block := r.block
		r.mu.Unlock()
		if block != nil {
			select {
			case <-block:
			case <-req.Context().Done():
				return
			}
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) deliveries() []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]delivery(nil), r.got...)
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func webhook(name, url string, events ...string) config.WebhookConfig {
	return config.WebhookConfig{Name: name, URL: url, Events: events}
}

func newNotifier(t *testing.T, opts Options, hooks ...config.WebhookConfig) *Notifier {
	t.Helper()
	if opts.Backoff == 0 {
		opts.Backoff = time.Millisecond
	}
	n := New(&config.NotificationsConfig{Webhooks: hooks}, opts)
	if n == nil {
		t.Fatal("New returned nil")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		n.Close(ctx)
	})
	return n
}

func deliveries(name, event, outcome string) float64 {
	m := &dto.Metric{}
	if err := metrics.WebhookDeliveriesTotal.WithLabelValues(name, event, outcome).Write(m); err != nil {
		panic(err)
	}
	return m.GetCounter().GetValue()
}

func TestDelivery_SignedPayload(t *testing.T) {
	t.Setenv("NOTIFY_TEST_SECRET", "s3cret")
	rcv := newReceiver(t)
	w := webhook("signed", rcv.URL, config.WebhookEventCircuitOpened)
	w.Secret = "${NOTIFY_TEST_SECRET}"
	n := newNotifier(t, Options{ExpandEnv: true}, w)

	at := time.Date(2026, 10, 11, 12, 0, 0, 0, time.FixedZone("x", 3600))
	n.Publish(Event{Type: config.WebhookEventCircuitOpened, Time: at, Target: "openai", Provider: "openai",
		Details: map[string]any{"from": "closed", "to": "open"}})
	waitFor(t, "a delivery", func() bool { return len(rcv.deliveries()) == 1 })

	d := rcv.deliveries()[0]
	ts := d.header.Get(HeaderTimestamp)
	if ts == "" {
		t.Fatal("no timestamp header")
	}
	want := Sign([]byte("s3cret"), ts, d.body)
	if got := d.header.Get(HeaderSignature); !hmac.Equal([]byte(got), []byte(want)) {
		t.Fatalf("signature = %q, want %q", got, want)
	}
	if !strings.HasPrefix(want, "sha256=") {
		t.Fatalf("signature %q lacks the sha256= prefix", want)
	}
	// A receiver verifying with the wrong secret, or against a tampered body,
	// must not match.
	if Sign([]byte("other"), ts, d.body) == want || Sign([]byte("s3cret"), ts, append(d.body, ' ')) == want {
		t.Fatal("signature does not depend on the secret and body")
	}
	if got := d.header.Get(HeaderEvent); got != config.WebhookEventCircuitOpened {
		t.Errorf("%s = %q", HeaderEvent, got)
	}
	if d.header.Get(HeaderDelivery) == "" {
		t.Errorf("no %s header", HeaderDelivery)
	}

	var p Payload
	if err := json.Unmarshal(d.body, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.Event != config.WebhookEventCircuitOpened || p.Target != "openai" || p.Provider != "openai" {
		t.Errorf("payload = %+v", p)
	}
	if p.Time != "2026-10-11T11:00:00Z" {
		t.Errorf("time = %q, want UTC RFC3339", p.Time)
	}
	if p.Details["to"] != "open" {
		t.Errorf("details = %v", p.Details)
	}
}

func TestDelivery_UnsignedWithoutSecret(t *testing.T) {
	rcv := newReceiver(t)
	n := newNotifier(t, Options{}, webhook("unsigned", rcv.URL, config.WebhookEventCircuitClosed))
	n.Publish(Event{Type: config.WebhookEventCircuitClosed})
	waitFor(t, "a delivery", func() bool { return len(rcv.deliveries()) == 1 })
	if got := rcv.deliveries()[0].header.Get(HeaderSignature); got != "" {
		t.Fatalf("unsigned webhook sent %s %q", HeaderSignature, got)
	}
}

func TestDelivery_OnlySubscribedEvents(t *testing.T) {
	rcv := newReceiver(t)
	n := newNotifier(t, Options{}, webhook("subset", rcv.URL, config.WebhookEventCircuitOpened))
	if n.Wants(config.WebhookEventBudgetExceeded) {
		t.Fatal("Wants reports an event no webhook subscribes to")
	}
	n.Publish(Event{Type: config.WebhookEventBudgetExceeded})
	n.Publish(Event{Type: config.WebhookEventCircuitOpened})
	waitFor(t, "a delivery", func() bool { return len(rcv.deliveries()) == 1 })
	time.Sleep(20 * time.Millisecond)
	if got := len(rcv.deliveries()); got != 1 {
		t.Fatalf("receiver saw %d deliveries, want only the subscribed one", got)
	}
}

func TestDelivery_4xxIsNotRetried(t *testing.T) {
	rcv := newReceiver(t, http.StatusBadRequest)
	n := newNotifier(t, Options{MaxAttempts: 5}, webhook("refuses", rcv.URL, config.WebhookEventCircuitOpened))
	before := deliveries("refuses", config.WebhookEventCircuitOpened, OutcomeRejected)

	n.Publish(Event{Type: config.WebhookEventCircuitOpened})
	waitFor(t, "the refusal to be counted", func() bool {
		return deliveries("refuses", config.WebhookEventCircuitOpened, OutcomeRejected) == before+1
	})
	time.Sleep(30 * time.Millisecond)
	if got := len(rcv.deliveries()); got != 1 {
		t.Fatalf("a 400 was attempted %d times, want 1", got)
	}
}

func TestDelivery_5xxIsRetriedWithTheSameDeliveryID(t *testing.T) {
	rcv := newReceiver(t, http.StatusServiceUnavailable, http.StatusBadGateway)
	n := newNotifier(t, Options{MaxAttempts: 3}, webhook("flaky", rcv.URL, config.WebhookEventCircuitOpened))
	before := deliveries("flaky", config.WebhookEventCircuitOpened, OutcomeDelivered)

	n.Publish(Event{Type: config.WebhookEventCircuitOpened})
	waitFor(t, "the delivery to succeed", func() bool {
		return deliveries("flaky", config.WebhookEventCircuitOpened, OutcomeDelivered) == before+1
	})
	got := rcv.deliveries()
	if len(got) != 3 {
		t.Fatalf("attempts = %d, want 3 (two 5xx, then 200)", len(got))
	}
	id := got[0].header.Get(HeaderDelivery)
	for i, d := range got {
		if d.header.Get(HeaderDelivery) != id {
			t.Fatalf("attempt %d carried a different delivery id", i+1)
		}
	}
}

func TestDelivery_RetriesAreBounded(t *testing.T) {
	rcv := newReceiver(t, 500, 500, 500, 500, 500)
	n := newNotifier(t, Options{MaxAttempts: 2}, webhook("down", rcv.URL, config.WebhookEventCircuitOpened))
	before := deliveries("down", config.WebhookEventCircuitOpened, OutcomeFailed)

	n.Publish(Event{Type: config.WebhookEventCircuitOpened})
	waitFor(t, "the failure to be counted", func() bool {
		return deliveries("down", config.WebhookEventCircuitOpened, OutcomeFailed) == before+1
	})
	if got := len(rcv.deliveries()); got != 2 {
		t.Fatalf("attempts = %d, want MaxAttempts (2)", got)
	}
}

func TestDelivery_TransportErrorIsRetried(t *testing.T) {
	// A server that is closed refuses connections: a transport error, which is
	// retried like a 5xx.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	n := newNotifier(t, Options{MaxAttempts: 2}, webhook("unreachable", url, config.WebhookEventCircuitOpened))
	before := deliveries("unreachable", config.WebhookEventCircuitOpened, OutcomeFailed)
	n.Publish(Event{Type: config.WebhookEventCircuitOpened})
	waitFor(t, "the failure to be counted", func() bool {
		return deliveries("unreachable", config.WebhookEventCircuitOpened, OutcomeFailed) == before+1
	})
}

func TestQueueOverflow_DropsOldestAndCounts(t *testing.T) {
	rcv := newReceiver(t)
	rcv.block = make(chan struct{})
	n := newNotifier(t, Options{QueueSize: 2}, webhook("overflow", rcv.URL, config.WebhookEventCircuitOpened))
	before := deliveries("overflow", config.WebhookEventCircuitOpened, OutcomeDropped)

	// The first event is taken by the worker and held by the receiver; the
	// queue then holds two, and every event past that pushes out the oldest.
	n.Publish(Event{Type: config.WebhookEventCircuitOpened, Target: "e0"})
	waitFor(t, "the first delivery to start", func() bool { return len(rcv.deliveries()) == 1 })
	for i := 1; i <= 5; i++ {
		n.Publish(Event{Type: config.WebhookEventCircuitOpened, Target: "e" + string(rune('0'+i))})
	}
	if got := deliveries("overflow", config.WebhookEventCircuitOpened, OutcomeDropped) - before; got != 3 {
		t.Fatalf("dropped = %v, want 3", got)
	}
	close(rcv.block)
	waitFor(t, "the queue to drain", func() bool { return len(rcv.deliveries()) == 3 })

	got := rcv.deliveries()
	targets := make([]string, 0, len(got))
	for _, d := range got {
		var p Payload
		_ = json.Unmarshal(d.body, &p)
		targets = append(targets, p.Target)
	}
	if strings.Join(targets, ",") != "e0,e4,e5" {
		t.Fatalf("delivered %v, want the newest events kept (e0,e4,e5)", targets)
	}
}

func TestPublish_NeverBlocksOnAStalledReceiver(t *testing.T) {
	rcv := newReceiver(t)
	rcv.block = make(chan struct{})
	defer close(rcv.block)
	n := newNotifier(t, Options{QueueSize: 4}, webhook("stalled", rcv.URL, config.WebhookEventCircuitOpened))

	done := make(chan struct{})
	go func() {
		for range 1000 {
			n.Publish(Event{Type: config.WebhookEventCircuitOpened})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked behind a stalled receiver")
	}
}

func TestClose_DrainsQueuedEvents(t *testing.T) {
	rcv := newReceiver(t)
	n := New(&config.NotificationsConfig{Webhooks: []config.WebhookConfig{
		webhook("drain", rcv.URL, config.WebhookEventCircuitOpened),
	}}, Options{Backoff: time.Millisecond})
	for range 5 {
		n.Publish(Event{Type: config.WebhookEventCircuitOpened})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n.Close(ctx)
	if got := len(rcv.deliveries()); got != 5 {
		t.Fatalf("delivered %d of 5 queued events before Close returned", got)
	}
	// Closed: further events are refused, not queued.
	n.Publish(Event{Type: config.WebhookEventCircuitOpened})
	time.Sleep(20 * time.Millisecond)
	if got := len(rcv.deliveries()); got != 5 {
		t.Fatalf("an event published after Close was delivered")
	}
}

func TestClose_GraceBoundAbandonsAndLeaksNothing(t *testing.T) {
	baseline := runtime.NumGoroutine()

	rcv := newReceiver(t)
	rcv.block = make(chan struct{})
	defer close(rcv.block)
	n := New(&config.NotificationsConfig{Webhooks: []config.WebhookConfig{
		webhook("hung", rcv.URL, config.WebhookEventCircuitOpened),
	}}, Options{Backoff: time.Millisecond})
	before := deliveries("hung", config.WebhookEventCircuitOpened, OutcomeAbandoned)
	for range 3 {
		n.Publish(Event{Type: config.WebhookEventCircuitOpened})
	}
	waitFor(t, "the first delivery to start", func() bool { return len(rcv.deliveries()) == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	n.Close(ctx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Close took %v against a 50ms grace", elapsed)
	}
	if got := deliveries("hung", config.WebhookEventCircuitOpened, OutcomeAbandoned) - before; got != 3 {
		t.Fatalf("abandoned = %v, want 3 (one in flight, two queued)", got)
	}

	// Every worker has exited when Close returns; only the test server's own
	// connection goroutines may linger briefly.
	rcv.CloseClientConnections()
	waitFor(t, "goroutines to return to baseline", func() bool {
		return runtime.NumGoroutine() <= baseline+2
	})
}

func TestNew_UnresolvableWebhookIsLeftOut(t *testing.T) {
	rcv := newReceiver(t)
	cfg := &config.NotificationsConfig{Webhooks: []config.WebhookConfig{
		webhook("missing-var", "${NOTIFY_TEST_UNSET_URL}", config.WebhookEventCircuitOpened),
		webhook("ok", rcv.URL, config.WebhookEventCircuitOpened),
	}}
	n := New(cfg, Options{ExpandEnv: true})
	if n == nil {
		t.Fatal("one bad webhook disabled them all")
	}
	defer n.Close(context.Background())
	if len(n.sinks) != 1 || n.sinks[0].name != "ok" {
		t.Fatalf("sinks = %d, want only the resolvable one", len(n.sinks))
	}
}

func TestNew_ResolvesURLAtConstruction(t *testing.T) {
	rcv := newReceiver(t)
	t.Setenv("NOTIFY_TEST_URL", rcv.URL)
	n := newNotifier(t, Options{ExpandEnv: true}, webhook("env", "${NOTIFY_TEST_URL}", config.WebhookEventCircuitOpened))
	n.Publish(Event{Type: config.WebhookEventCircuitOpened})
	waitFor(t, "a delivery", func() bool { return len(rcv.deliveries()) == 1 })
}

func TestNew_ReferencesStayLiteralWithoutExpansion(t *testing.T) {
	t.Setenv("NOTIFY_TEST_URL", "http://127.0.0.1:1")
	cfg := &config.NotificationsConfig{Webhooks: []config.WebhookConfig{
		webhook("env", "${NOTIFY_TEST_URL}", config.WebhookEventCircuitOpened),
	}}
	if n := New(cfg, Options{ExpandEnv: false}); n != nil {
		n.Close(context.Background())
		t.Fatal("a ${VAR} url was resolved with expansion disabled")
	}
}

func TestNew_NothingConfigured(t *testing.T) {
	if New(nil, Options{}) != nil || New(&config.NotificationsConfig{}, Options{}) != nil {
		t.Fatal("New built a notifier with no webhooks")
	}
	var n *Notifier
	n.Publish(Event{Type: config.WebhookEventCircuitOpened})
	n.ObserveAttempt("t", "p", true)
	n.Close(context.Background())
	if n.Wants(config.WebhookEventCircuitOpened) || n.WantsAttempts() {
		t.Fatal("a nil notifier wants events")
	}
}

func TestErrText_OmitsURL(t *testing.T) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://127.0.0.1:1/hooks/T000/SECRET-TOKEN", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Skip("port 1 accepted a connection")
	}
	if strings.Contains(errText(err), "SECRET-TOKEN") {
		t.Fatalf("errText leaked the URL: %q", errText(err))
	}
}

func TestObserveAttempt_FiresOncePerRise(t *testing.T) {
	rcv := newReceiver(t)
	now := time.Unix(1_800_000_000, 0)
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	w := webhook("errors", rcv.URL, config.WebhookEventErrorRateHigh)
	w.ErrorRate = &config.ErrorRateConfig{Threshold: 0.5, Window: "1m", MinRequests: 4}
	n := newNotifier(t, Options{Now: func() time.Time { return time.Unix(0, clock.Load()) }}, w)

	// Below the minimum count nothing is judged, however bad the rate.
	for range 3 {
		n.ObserveAttempt("openai", "openai", true)
	}
	// The fourth attempt reaches the minimum at a 100% rate: fire.
	n.ObserveAttempt("openai", "openai", true)
	// Still high: no repeat.
	for range 10 {
		n.ObserveAttempt("openai", "openai", true)
	}
	waitFor(t, "one delivery", func() bool { return len(rcv.deliveries()) == 1 })

	var p Payload
	_ = json.Unmarshal(rcv.deliveries()[0].body, &p)
	if p.Event != config.WebhookEventErrorRateHigh || p.Target != "openai" || p.Details["requests"] != float64(4) {
		t.Fatalf("payload = %+v", p)
	}

	// Recover below the threshold, then rise again: a second event.
	for range 30 {
		n.ObserveAttempt("openai", "openai", false)
	}
	for range 40 {
		n.ObserveAttempt("openai", "openai", true)
	}
	waitFor(t, "a second delivery", func() bool { return len(rcv.deliveries()) == 2 })

	// A different target has its own window.
	for range 4 {
		n.ObserveAttempt("anthropic", "anthropic", true)
	}
	waitFor(t, "the other target's delivery", func() bool { return len(rcv.deliveries()) == 3 })
	time.Sleep(20 * time.Millisecond)
	if got := len(rcv.deliveries()); got != 3 {
		t.Fatalf("deliveries = %d, want 3", got)
	}
}

func TestRateTracker_WindowForgetsOldSamples(t *testing.T) {
	tr := newRateTracker(config.ErrorRateConfig{Threshold: 0.5, Window: "10s", MinRequests: 2})
	t0 := time.Unix(1_800_000_000, 0)
	tr.observe("x", true, t0)
	if _, fire := tr.observe("x", true, t0.Add(time.Second)); !fire {
		t.Fatal("two failures in the window did not fire")
	}
	// Eleven seconds later both samples are outside the window: one new
	// failure is below the minimum, which re-arms the event.
	if r, fire := tr.observe("x", true, t0.Add(12*time.Second)); fire || r.total != 1 {
		t.Fatalf("stale samples counted: total=%d fire=%v", r.total, fire)
	}
	if _, fire := tr.observe("x", true, t0.Add(13*time.Second)); !fire {
		t.Fatal("a new rise after the window emptied did not fire")
	}
}
