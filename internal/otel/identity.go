package otel

import (
	"context"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ferro-labs/ai-gateway/observability"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
)

// Request identity inputs the HTTP layer reads. Headers outrank baggage
// because a header is set for this hop on purpose, while baggage is whatever
// an upstream service happened to propagate.
const (
	userIDHeader      = "X-User-ID"
	sessionIDHeader   = "X-Session-ID"
	baggageHeader     = "baggage"
	baggageUserKey    = "user.id"
	baggageSessionKey = "session.id"

	// maxIdentityValueLen bounds one id. An id is an opaque token, not a
	// document; a longer value is dropped rather than truncated, because a
	// truncated id names a different user than the caller did.
	maxIdentityValueLen = 256
)

// requestIdentityFromHeaders reads the end-user and session ids a request
// carries. Baggage is parsed directly with the W3C baggage propagator rather
// than through the global propagator, so it is read identically whether or not
// tracing is enabled — the identity feeds the request log too.
func requestIdentityFromHeaders(ctx context.Context, h http.Header) observability.RequestIdentity {
	id := observability.RequestIdentity{
		User:      IdentityValue(h.Get(userIDHeader)),
		SessionID: IdentityValue(h.Get(sessionIDHeader)),
	}
	if (id.User != "" && id.SessionID != "") || h.Get(baggageHeader) == "" {
		return id
	}
	bag := baggage.FromContext(propagation.Baggage{}.Extract(ctx, propagation.HeaderCarrier(h)))
	if id.User == "" {
		id.User = IdentityValue(bag.Member(baggageUserKey).Value())
	}
	if id.SessionID == "" {
		id.SessionID = IdentityValue(bag.Member(baggageSessionKey).Value())
	}
	return id
}

// IdentityValue returns v trimmed, or "" when v cannot be an id: empty, longer
// than maxIdentityValueLen, carrying a control character, or not UTF-8. It is
// the one rule applied to every identity input — the HTTP headers here and the
// gateway core's own overlay of the OpenAI body `user` field — so a value a
// caller cannot get past a header can not reach the same field through the
// body either.
//
// A header value may hold any byte from 0x80 up, and Go's server accepts it, so
// UTF-8 is checked rather than assumed. Such a value is dropped, not repaired,
// for the reason a long one is: two different byte strings would repair to the
// same id. Kept, it reached the span as-is and failed the OTLP export of every
// span batched with it.
//
// Control characters are judged per rune, not per byte. A byte check sees only
// the ASCII ones, while the C1 controls U+0080–U+009F — NEL among them, which
// many line-oriented readers split a line on — arrive as two bytes from 0x80 up
// and passed it, reaching the span and the request-log row from a header, a
// percent-encoded baggage entry or the body `user` alike.
func IdentityValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > maxIdentityValueLen || !utf8.ValidString(v) {
		return ""
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return ""
		}
	}
	return v
}
