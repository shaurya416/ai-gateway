// Package notify delivers gateway events to the webhooks configured under
// notifications.webhooks.
//
// Delivery never runs on the request path. Publish copies an event into a
// bounded queue per webhook and returns; one worker per webhook posts it. A
// queue that is full makes way for the newest event by dropping its oldest,
// and counts the drop, because under an incident the latest state is the one
// worth sending. A receiver answering 5xx, or not answering, is retried a
// bounded number of times with exponential backoff; any other non-2xx answer
// is final, since repeating a request the receiver refused gets the same
// answer. Close drains what is queued within a grace bound and then abandons
// the rest, so shutdown can neither hang on a dead receiver nor leak a worker.
//
// A payload names the event, when it happened, the target and provider it is
// about, and a small details object. It never carries a credential, a request
// body, or prompt text.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/envref"
	"github.com/ferro-labs/ai-gateway/internal/httpclient"
	"github.com/ferro-labs/ai-gateway/internal/version"
	"github.com/ferro-labs/ai-gateway/pkg/logger"
	"github.com/ferro-labs/ai-gateway/pkg/metrics"
)

// Headers set on every delivery.
const (
	HeaderEvent     = "X-Ferro-Event"
	HeaderDelivery  = "X-Ferro-Delivery"
	HeaderTimestamp = "X-Ferro-Timestamp"
	HeaderSignature = "X-Ferro-Signature"
)

// Delivery outcomes, the values of the outcome label on
// gateway_webhook_deliveries_total.
const (
	OutcomeDelivered = "delivered"
	OutcomeRejected  = "rejected"
	OutcomeFailed    = "failed"
	OutcomeDropped   = "dropped"
	OutcomeAbandoned = "abandoned"
)

// Defaults for the delivery policy. Options can override them.
const (
	DefaultQueueSize   = 256
	DefaultMaxAttempts = 3
	DefaultBackoff     = time.Second
	maxBackoff         = 30 * time.Second
	// maxDrainBody bounds how much of a receiver's answer is read so the
	// connection can be reused. The answer itself is never used.
	maxDrainBody = 64 << 10
)

// Event is one notification.
type Event struct {
	// Type is one of config.WebhookEvents().
	Type string
	// Time is when the event happened.
	Time time.Time
	// Target is the routing target the event is about, when there is one.
	Target string
	// Provider is the target's provider, when it is known.
	Provider string
	// Details carries event-specific values. It is read, never written, once
	// published, and must hold only JSON-encodable scalars.
	Details map[string]any
}

// Payload is the JSON body a webhook receives.
type Payload struct {
	Event    string         `json:"event"`
	Time     string         `json:"time"`
	Target   string         `json:"target,omitempty"`
	Provider string         `json:"provider,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
}

// Options tunes a Notifier. The zero value is the production policy.
type Options struct {
	// Logger receives delivery failures. Nil means logger.Default().
	Logger *logger.Logger
	// ExpandEnv resolves ${VAR} references in each webhook's url and secret
	// from the process environment. Without it a reference stays literal, so a
	// webhook whose url or secret is one cannot be built and is disabled.
	ExpandEnv bool
	// QueueSize bounds each webhook's pending events. Zero means
	// DefaultQueueSize.
	QueueSize int
	// MaxAttempts bounds delivery attempts per event, the first included.
	// Zero means DefaultMaxAttempts.
	MaxAttempts int
	// Backoff is the wait before the first retry; each later retry doubles
	// it. Zero means DefaultBackoff.
	Backoff time.Duration
	// Now is the clock events and error rates are measured on. Nil means
	// time.Now.
	Now func() time.Time
}

// Notifier fans events out to the configured webhooks.
//
// A nil *Notifier is valid and does nothing, which is what a gateway with no
// webhooks configured holds.
type Notifier struct {
	sinks   []*sink
	byEvent map[string][]*sink
	rated   []*sink // the sinks subscribed to error_rate.high
	now     func() time.Time

	// ctx parents every delivery. Close cancels it once the grace runs out,
	// which aborts a delivery in flight and a retry wait in progress.
	ctx       context.Context
	cancel    context.CancelFunc
	closed    atomic.Bool
	closeOnce sync.Once
}

// New builds a Notifier for cfg and starts one worker per webhook. A webhook
// that cannot be built — a ${VAR} that does not resolve, or a url that does
// not resolve to http or https — is logged and left out, so a missing
// notification credential never stops the gateway serving. New returns nil
// when no webhook could be built.
func New(cfg *config.NotificationsConfig, opts Options) *Notifier {
	if cfg == nil || len(cfg.Webhooks) == 0 {
		return nil
	}
	log := opts.Logger
	if log == nil {
		log = logger.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &Notifier{byEvent: make(map[string][]*sink), now: now, ctx: ctx, cancel: cancel}
	for _, w := range cfg.Webhooks {
		s, err := newSink(w, opts, log)
		if err != nil {
			log.Error("notifications: webhook disabled", "webhook", w.Name, "error", err)
			continue
		}
		n.sinks = append(n.sinks, s)
		for _, ev := range w.Events {
			n.byEvent[ev] = append(n.byEvent[ev], s)
		}
		if s.rate != nil {
			n.rated = append(n.rated, s)
		}
	}
	if len(n.sinks) == 0 {
		cancel()
		return nil
	}
	for _, s := range n.sinks {
		go s.run(ctx)
	}
	return n
}

// Wants reports whether any webhook subscribes to eventType, so a caller can
// skip building an event nobody receives.
func (n *Notifier) Wants(eventType string) bool {
	return n != nil && len(n.byEvent[eventType]) > 0
}

// WantsAttempts reports whether any webhook measures an error rate, and so
// whether ObserveAttempt does anything.
func (n *Notifier) WantsAttempts() bool {
	return n != nil && len(n.rated) > 0
}

// Publish queues ev for every webhook subscribed to its type and returns
// without waiting for any delivery. It never blocks on a receiver. After Close
// it does nothing.
func (n *Notifier) Publish(ev Event) {
	if n == nil || n.closed.Load() {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = n.now()
	}
	for _, s := range n.byEvent[ev.Type] {
		s.enqueue(ev)
	}
}

// ObserveAttempt records one routing attempt against target for every webhook
// measuring an error rate, and queues error_rate.high to each one whose
// threshold the attempt carries the target's rate up to. failed is whether
// the attempt counts as an error; an attempt that is not a sample at all must
// not be reported.
func (n *Notifier) ObserveAttempt(target, provider string, failed bool) {
	if n == nil || n.closed.Load() {
		return
	}
	now := n.now()
	for _, s := range n.rated {
		reading, fire := s.rate.observe(target, failed, now)
		if !fire {
			continue
		}
		s.enqueue(Event{
			Type:     config.WebhookEventErrorRateHigh,
			Time:     now,
			Target:   target,
			Provider: provider,
			Details: map[string]any{
				"error_rate":   reading.rate(),
				"errors":       reading.errors,
				"requests":     reading.total,
				"threshold":    s.rate.threshold,
				"window":       s.rate.window.String(),
				"min_requests": s.rate.minRequests,
			},
		})
	}
}

// Close stops accepting events and delivers what is already queued, waiting
// at most until ctx is done. Whatever is still pending then — an attempt in
// flight, a retry waiting out its backoff, events not yet started — is
// abandoned and counted. Close returns only once every worker has exited, and
// is safe to call more than once.
func (n *Notifier) Close(ctx context.Context) {
	if n == nil {
		return
	}
	n.closeOnce.Do(func() {
		n.closed.Store(true)
		for _, s := range n.sinks {
			s.stopAccepting()
		}
		done := make(chan struct{})
		go func() {
			for _, s := range n.sinks {
				<-s.done
			}
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			// Every wait a worker makes is on n.ctx, so cancelling it bounds
			// the wait below to the time it takes to unwind.
			n.cancel()
			<-done
		}
		n.cancel()
	})
}

// sink is one webhook: its resolved endpoint, its queue and its worker.
type sink struct {
	name        string
	url         string
	secret      []byte
	client      *http.Client
	maxAttempts int
	backoff     time.Duration
	log         *logger.Logger
	rate        *rateTracker // nil unless subscribed to error_rate.high

	mu      sync.Mutex
	buf     []Event // ring buffer of pending events
	head    int
	count   int
	stopped bool

	wake chan struct{} // capacity 1: "the queue may be non-empty"
	stop chan struct{} // closed by stopAccepting
	done chan struct{} // closed when the worker exits
}

func newSink(w config.WebhookConfig, opts Options, log *logger.Logger) (*sink, error) {
	rawURL, secret := w.URL, w.Secret
	if opts.ExpandEnv {
		var err error
		if rawURL, err = envref.Expand(w.URL); err != nil {
			return nil, fmt.Errorf("url: %w", err)
		}
		if secret, err = envref.Expand(w.Secret); err != nil {
			return nil, fmt.Errorf("secret: %w", err)
		}
	} else if w.Secret != "" {
		return nil, errors.New("secret is a ${VAR} reference and environment expansion is disabled")
	}
	if err := config.ValidateWebhookURL(rawURL); err != nil {
		// The resolved URL can carry a token, so it is never quoted.
		return nil, fmt.Errorf("url %w", err)
	}
	if w.Secret != "" && secret == "" {
		return nil, errors.New("secret resolves to an empty value")
	}

	queueSize := opts.QueueSize
	if queueSize <= 0 {
		queueSize = DefaultQueueSize
	}
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	backoff := opts.Backoff
	if backoff <= 0 {
		backoff = DefaultBackoff
	}
	s := &sink{
		name:        w.Name,
		url:         rawURL,
		client:      httpclient.New(w.WebhookTimeout()),
		maxAttempts: maxAttempts,
		backoff:     backoff,
		log:         log,
		buf:         make([]Event, queueSize),
		wake:        make(chan struct{}, 1),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	if secret != "" {
		s.secret = []byte(secret)
	}
	if w.ErrorRate != nil {
		s.rate = newRateTracker(*w.ErrorRate)
	}
	return s, nil
}

// enqueue adds ev to the queue, dropping the oldest pending event when it is
// full. It never blocks on the worker.
func (s *sink) enqueue(ev Event) {
	var dropped Event
	overflow := false
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	if s.count == len(s.buf) {
		dropped = s.buf[s.head]
		s.buf[s.head] = Event{}
		s.head = (s.head + 1) % len(s.buf)
		s.count--
		overflow = true
	}
	s.buf[(s.head+s.count)%len(s.buf)] = ev
	s.count++
	s.mu.Unlock()

	if overflow {
		s.record(dropped.Type, OutcomeDropped)
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// pop removes and returns the oldest pending event.
func (s *sink) pop() (Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count == 0 {
		return Event{}, false
	}
	ev := s.buf[s.head]
	s.buf[s.head] = Event{}
	s.head = (s.head + 1) % len(s.buf)
	s.count--
	return ev, true
}

// stopAccepting refuses further events and tells the worker to drain and
// exit. Setting stopped under the queue lock before closing stop means no
// event can land after the worker's final drain.
func (s *sink) stopAccepting() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	close(s.stop)
}

// run is the webhook's worker: it delivers queued events one at a time, in
// order, until the queue is drained after stopAccepting.
func (s *sink) run(ctx context.Context) {
	defer close(s.done)
	for {
		if ev, ok := s.pop(); ok {
			s.handle(ctx, ev)
			continue
		}
		select {
		case <-s.wake:
		case <-s.stop:
			for {
				ev, ok := s.pop()
				if !ok {
					return
				}
				s.handle(ctx, ev)
			}
		}
	}
}

func (s *sink) handle(ctx context.Context, ev Event) {
	if ctx.Err() != nil {
		s.record(ev.Type, OutcomeAbandoned)
		return
	}
	s.record(ev.Type, s.deliver(ctx, ev))
}

// deliver posts ev until the receiver accepts it, refuses it, or the retry
// budget is spent, and returns the outcome.
func (s *sink) deliver(ctx context.Context, ev Event) string {
	body, err := json.Marshal(Payload{
		Event:    ev.Type,
		Time:     ev.Time.UTC().Format(time.RFC3339),
		Target:   ev.Target,
		Provider: ev.Provider,
		Details:  ev.Details,
	})
	if err != nil {
		s.log.Error("notifications: event could not be encoded", "webhook", s.name, "event", ev.Type, "error", err)
		return OutcomeFailed
	}
	id := deliveryID()
	wait := s.backoff
	for attempt := 1; ; attempt++ {
		status, err := s.post(ctx, ev.Type, id, body)
		switch {
		case err == nil && status >= 200 && status < 300:
			return OutcomeDelivered
		case ctx.Err() != nil:
			return OutcomeAbandoned
		case err == nil && status < 500:
			s.log.Warn("notifications: receiver refused the event; not retried",
				"webhook", s.name, "event", ev.Type, "status", status)
			return OutcomeRejected
		}
		if attempt >= s.maxAttempts {
			s.log.Warn("notifications: delivery failed after retries",
				"webhook", s.name, "event", ev.Type, "attempts", attempt, "status", status, "error", errText(err))
			return OutcomeFailed
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return OutcomeAbandoned
		}
		wait = min(wait*2, maxBackoff)
	}
}

// post makes one delivery attempt and returns the receiver's status, or the
// error that kept it from answering.
func (s *sink) post(ctx context.Context, eventType, id string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ferrogw/"+version.Short())
	req.Header.Set(HeaderEvent, eventType)
	req.Header.Set(HeaderDelivery, id)
	req.Header.Set(HeaderTimestamp, timestamp)
	if s.secret != nil {
		req.Header.Set(HeaderSignature, Sign(s.secret, timestamp, body))
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBody))
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

func (s *sink) record(eventType, outcome string) {
	metrics.WebhookDeliveriesTotal.WithLabelValues(s.name, eventType, outcome).Inc()
}

// Sign returns the X-Ferro-Signature value for one delivery:
// "sha256=" + hex(HMAC-SHA256(secret, timestamp + "." + body)), where
// timestamp is the X-Ferro-Timestamp header. Covering the timestamp lets a
// receiver refuse a replayed delivery by its age.
func Sign(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// deliveryID returns a random id shared by every attempt of one delivery, so a
// receiver can recognise a retry of an event it already processed.
func deliveryID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// errText renders a delivery error without the URL. net/http quotes the
// request URL in every transport error, and a webhook URL can carry its token
// in the path or query.
func errText(err error) string {
	if err == nil {
		return ""
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err.Error()
	}
	return err.Error()
}
