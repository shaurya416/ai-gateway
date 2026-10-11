package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ferro-labs/ai-gateway/internal/envref"
)

// Webhook event names: the closed set notifications.webhooks[].events accepts.
const (
	// WebhookEventCircuitOpened fires when a target's circuit breaker opens.
	WebhookEventCircuitOpened = "circuit.opened"
	// WebhookEventCircuitHalfOpen fires when an open circuit admits its first
	// recovery probe window.
	WebhookEventCircuitHalfOpen = "circuit.half_open"
	// WebhookEventCircuitClosed fires when a half-open circuit closes again.
	WebhookEventCircuitClosed = "circuit.closed"
	// WebhookEventBudgetExceeded fires on a budget's first refusal for a key
	// since that key was last admitted.
	WebhookEventBudgetExceeded = "budget.exceeded"
	// WebhookEventErrorRateHigh fires when a target's error rate rises to its
	// webhook's configured threshold.
	WebhookEventErrorRateHigh = "error_rate.high"
)

// WebhookEvents returns the event names a webhook may subscribe to. Any other
// value in notifications.webhooks[].events is a load error.
func WebhookEvents() []string {
	return []string{
		WebhookEventCircuitOpened,
		WebhookEventCircuitHalfOpen,
		WebhookEventCircuitClosed,
		WebhookEventBudgetExceeded,
		WebhookEventErrorRateHigh,
	}
}

// Defaults applied when the corresponding webhook setting is omitted.
const (
	DefaultWebhookTimeout         = 5 * time.Second
	DefaultErrorRateWindow        = time.Minute
	DefaultErrorRateMinRequests   = 10
	maxWebhookNameLen             = 64
	webhookSecretReferenceExample = "${WEBHOOK_SECRET}" //nolint:gosec // G101: an example reference shown in an error, not a credential
)

// NotificationsConfig configures outbound alerts.
type NotificationsConfig struct {
	// Webhooks lists the endpoints events are posted to. Each subscribes to its
	// own events and is delivered through its own queue, so a slow or failing
	// receiver never delays another.
	Webhooks []WebhookConfig `json:"webhooks,omitempty" yaml:"webhooks,omitempty"`
}

// WebhookConfig is one webhook receiver.
type WebhookConfig struct {
	// Name identifies the webhook in logs and in the
	// gateway_webhook_deliveries_total metric. Required and unique.
	Name string `json:"name" yaml:"name"`
	// URL is the http or https endpoint events are POSTed to. It may be, or
	// contain, a ${VAR} reference, resolved when the gateway is constructed —
	// never at load — so a URL carrying a token in its path or query is never
	// stored or served by GET /admin/config. A URL with no reference is
	// checked at load.
	URL string `json:"url" yaml:"url"`
	// Events is the set of events this webhook receives. Each must be one of
	// WebhookEvents(); an unknown value is a load error.
	Events []string `json:"events" yaml:"events"`
	// Secret, when set, signs every delivery with HMAC-SHA256. It must be
	// written as a ${VAR} reference: a literal would be persisted to the config
	// history and is refused at load.
	Secret string `json:"secret,omitempty" yaml:"secret,omitempty"`
	// Timeout bounds one delivery attempt. Omitted means 5s.
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	// ErrorRate sets when error_rate.high fires. Required when Events lists
	// error_rate.high, and refused otherwise.
	ErrorRate *ErrorRateConfig `json:"error_rate,omitempty" yaml:"error_rate,omitempty"`
}

// ErrorRateConfig is the threshold an error_rate.high subscription fires at.
type ErrorRateConfig struct {
	// Threshold is the failing fraction of attempts, in (0, 1], at which the
	// event fires.
	Threshold float64 `json:"threshold" yaml:"threshold"`
	// Window is the sliding period the rate is measured over. Omitted means 1m.
	Window string `json:"window,omitempty" yaml:"window,omitempty"`
	// MinRequests is the fewest attempts in the window the rate is judged on,
	// so one failure out of one attempt is not a 100% error rate. Omitted
	// means 10.
	MinRequests int `json:"min_requests,omitempty" yaml:"min_requests,omitempty"`
}

// WebhookTimeout returns the delivery timeout the webhook resolves to. It
// assumes the config validated.
func (w WebhookConfig) WebhookTimeout() time.Duration {
	if w.Timeout == "" {
		return DefaultWebhookTimeout
	}
	d, err := time.ParseDuration(w.Timeout)
	if err != nil || d <= 0 {
		return DefaultWebhookTimeout
	}
	return d
}

// WindowDuration returns the window the error rate is measured over. It
// assumes the config validated.
func (e ErrorRateConfig) WindowDuration() time.Duration {
	if e.Window == "" {
		return DefaultErrorRateWindow
	}
	d, err := time.ParseDuration(e.Window)
	if err != nil || d <= 0 {
		return DefaultErrorRateWindow
	}
	return d
}

// MinimumRequests returns the fewest attempts the rate is judged on.
func (e ErrorRateConfig) MinimumRequests() int {
	if e.MinRequests <= 0 {
		return DefaultErrorRateMinRequests
	}
	return e.MinRequests
}

// ValidateWebhookURL checks a resolved webhook URL: an absolute http or https
// URL naming a host. It is the check the load applies to a URL with no ${VAR}
// reference and the one construction applies after resolving one.
func ValidateWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		// url.Error quotes the input, and a resolved webhook URL can carry a
		// token, so only the reason is kept.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("is not a URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("must be an http or https URL, got scheme %q", u.Scheme)
	}
	if u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("must name a host")
	}
	if u.Fragment != "" {
		return fmt.Errorf("must not carry a fragment; it is never sent")
	}
	return nil
}

// validateNotifications checks the notifications block.
func validateNotifications(n *NotificationsConfig) error {
	if n == nil {
		return nil
	}
	names := make(map[string]struct{}, len(n.Webhooks))
	for i, w := range n.Webhooks {
		where := fmt.Sprintf("notifications.webhooks[%d]", i)
		if w.Name == "" {
			return fmt.Errorf("%s.name must not be empty", where)
		}
		if strings.TrimSpace(w.Name) != w.Name {
			return fmt.Errorf("%s.name %q must not carry surrounding whitespace", where, w.Name)
		}
		if len(w.Name) > maxWebhookNameLen {
			return fmt.Errorf("%s.name must be at most %d bytes", where, maxWebhookNameLen)
		}
		if _, dup := names[w.Name]; dup {
			return fmt.Errorf("notifications.webhooks: name %q is listed more than once", w.Name)
		}
		names[w.Name] = struct{}{}
		where = fmt.Sprintf("notifications.webhooks[%s]", w.Name)
		if err := validateWebhook(where, w); err != nil {
			return err
		}
	}
	return nil
}

func validateWebhook(where string, w WebhookConfig) error {
	switch {
	case w.URL == "":
		return fmt.Errorf("%s.url must not be empty", where)
	case !envref.HasReference(w.URL):
		if err := ValidateWebhookURL(w.URL); err != nil {
			return fmt.Errorf("%s.url %w", where, err)
		}
	}

	if len(w.Events) == 0 {
		return fmt.Errorf("%s.events must list at least one of %s", where, strings.Join(WebhookEvents(), ", "))
	}
	known := WebhookEvents()
	seen := make(map[string]struct{}, len(w.Events))
	for _, ev := range w.Events {
		if !slices.Contains(known, ev) {
			return fmt.Errorf("%s.events: unknown event %q; known events are %s", where, ev, strings.Join(known, ", "))
		}
		if _, dup := seen[ev]; dup {
			return fmt.Errorf("%s.events: %q is listed more than once", where, ev)
		}
		seen[ev] = struct{}{}
	}

	if w.Secret != "" && !isWholeEnvReference(w.Secret) {
		return fmt.Errorf("%s.secret must be a ${VAR} reference such as %s; a literal secret would be stored in the config history", where, webhookSecretReferenceExample)
	}

	if w.Timeout != "" {
		d, err := time.ParseDuration(w.Timeout)
		if err != nil {
			return fmt.Errorf("%s.timeout %q is not a duration: %w", where, w.Timeout, err)
		}
		if d <= 0 {
			return fmt.Errorf("%s.timeout must be positive, got %q", where, w.Timeout)
		}
	}

	_, wantsErrorRate := seen[WebhookEventErrorRateHigh]
	switch {
	case wantsErrorRate && w.ErrorRate == nil:
		return fmt.Errorf("%s: events lists %s, which needs an error_rate block with a threshold", where, WebhookEventErrorRateHigh)
	case !wantsErrorRate && w.ErrorRate != nil:
		return fmt.Errorf("%s: error_rate is set but events does not list %s, so it would never fire", where, WebhookEventErrorRateHigh)
	case w.ErrorRate != nil:
		return validateErrorRate(where+".error_rate", *w.ErrorRate)
	}
	return nil
}

func validateErrorRate(where string, e ErrorRateConfig) error {
	if math.IsNaN(e.Threshold) || e.Threshold <= 0 || e.Threshold > 1 {
		return fmt.Errorf("%s.threshold must be a fraction in (0, 1], got %v", where, e.Threshold)
	}
	if e.Window != "" {
		d, err := time.ParseDuration(e.Window)
		if err != nil {
			return fmt.Errorf("%s.window %q is not a duration: %w", where, e.Window, err)
		}
		if d <= 0 {
			return fmt.Errorf("%s.window must be positive, got %q", where, e.Window)
		}
	}
	if e.MinRequests < 0 {
		return fmt.Errorf("%s.min_requests cannot be negative, got %d (omit it for the default of %d)", where, e.MinRequests, DefaultErrorRateMinRequests)
	}
	return nil
}

// isWholeEnvReference reports whether s is exactly one ${VAR} reference.
func isWholeEnvReference(s string) bool {
	return len(s) > 3 && strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}") && isEnvName(s[2:len(s)-1])
}

func isEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_' || ('A' <= r && r <= 'Z') || ('a' <= r && r <= 'z'):
		case i > 0 && '0' <= r && r <= '9':
		default:
			return false
		}
	}
	return true
}
