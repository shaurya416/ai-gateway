package otel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/observability"
)

func identityThroughMiddleware(t *testing.T, set func(h http.Header)) observability.RequestIdentity {
	t.Helper()
	var seen observability.RequestIdentity
	h := Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = observability.RequestIdentityFromContext(r.Context())
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", nil)
	set(req.Header)
	h.ServeHTTP(httptest.NewRecorder(), req)
	return seen
}

func TestMiddleware_IdentityFromHeaders(t *testing.T) {
	got := identityThroughMiddleware(t, func(h http.Header) {
		h.Set("X-User-ID", " user-42 ")
		h.Set("X-Session-ID", "sess-7")
	})
	if got.User != "user-42" || got.SessionID != "sess-7" {
		t.Fatalf("identity = %+v, want user-42 / sess-7 (trimmed)", got)
	}
	if got.Metadata != nil {
		t.Fatalf("metadata = %v, want nil: the HTTP layer reads no metadata", got.Metadata)
	}
}

func TestMiddleware_IdentityFromBaggageWithoutPropagatorInstalled(t *testing.T) {
	// No installPropagator() here on purpose: baggage must be read whether or
	// not tracing is on, since the identity feeds the request log too.
	got := identityThroughMiddleware(t, func(h http.Header) {
		h.Set("baggage", "user.id=user%2042,session.id=sess-7,other=x")
	})
	if got.User != "user 42" || got.SessionID != "sess-7" {
		t.Fatalf("identity = %+v, want percent-decoded user / sess-7 from baggage", got)
	}
}

func TestMiddleware_HeaderOutranksBaggage(t *testing.T) {
	got := identityThroughMiddleware(t, func(h http.Header) {
		h.Set("X-User-ID", "from-header")
		h.Set("baggage", "user.id=from-baggage,session.id=sess-baggage")
	})
	if got.User != "from-header" {
		t.Errorf("user = %q, want the header value", got.User)
	}
	if got.SessionID != "sess-baggage" {
		t.Errorf("session = %q, want the baggage value when the header is absent", got.SessionID)
	}
}

func TestMiddleware_IdentityRejectsUnusableValues(t *testing.T) {
	cases := map[string]string{
		"too long":          strings.Repeat("a", maxIdentityValueLen+1),
		"control character": "user\x00id",
		"blank":             "   ",
		// Go's HTTP server accepts any byte >= 0x80 in a header value. Kept,
		// such a value reached the span and the request-log row as-is, and an
		// OTLP export cannot marshal a string that is not UTF-8.
		"not UTF-8": "user\xffid",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			got := identityThroughMiddleware(t, func(h http.Header) { h.Set("X-User-ID", value) })
			if !got.IsZero() {
				t.Fatalf("identity = %+v, want zero for an unusable header value", got)
			}
		})
	}
}

func TestMiddleware_NoIdentityInputsLeavesContextZero(t *testing.T) {
	got := identityThroughMiddleware(t, func(http.Header) {})
	if !got.IsZero() {
		t.Fatalf("identity = %+v, want zero", got)
	}
}

// TestIdentityValue_RejectsC1ControlCharacters is the regression test for the
// control-character rule reading bytes instead of runes. The C1 controls
// U+0080–U+009F are UTF-8 encoded as two bytes from 0x80 up, so a byte check
// never saw them: NEL (U+0085), which many line-oriented readers split a line
// on, passed from a header, from a percent-encoded baggage entry, and — through
// the same rule — from the body `user` field.
func TestIdentityValue_RejectsC1ControlCharacters(t *testing.T) {
	for name, value := range map[string]string{
		"next line":           "ali\u0085ce",
		"single shift three":  "ali\u008fce",
		"application command": "ali\u009fce",
		"padding character":   "ali\u0080ce",
	} {
		t.Run(name, func(t *testing.T) {
			if got := IdentityValue(value); got != "" {
				t.Errorf("IdentityValue(%q) = %q, want it dropped", value, got)
			}
			got := identityThroughMiddleware(t, func(h http.Header) { h.Set("X-User-ID", value) })
			if !got.IsZero() {
				t.Errorf("identity from X-User-ID = %+v, want zero", got)
			}
		})
	}

	got := identityThroughMiddleware(t, func(h http.Header) {
		h.Set("baggage", "user.id=ali%C2%85ce,session.id=sess%C2%85ion")
	})
	if !got.IsZero() {
		t.Errorf("identity from baggage = %+v, want zero", got)
	}

	// Ordinary non-ASCII text is still an id.
	if got := IdentityValue("José-Ünal"); got != "José-Ünal" {
		t.Errorf("IdentityValue(%q) = %q, want it kept", "José-Ünal", got)
	}
}
