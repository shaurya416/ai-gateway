package middleware

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/ferro-labs/ai-gateway/internal/apierror"
	"github.com/ferro-labs/ai-gateway/pkg/metrics"
	"github.com/ferro-labs/ai-gateway/pkg/ratelimit"
)

// retryAfterSeconds is the Retry-After hint sent with every 429 this middleware
// writes.
//
// The header is not decoration. RFC 9110 §10.2.3 defines it and RFC 6585
// recommends it on 429, and the SDKs on the other side of this gateway read it:
// the OpenAI Go client prefers Retry-After-Ms and falls back to Retry-After,
// and the Anthropic and OpenRouter clients behave the same way. A 429 without
// it leaves those clients on a fixed short delay, so the limiter's own
// rejections are what produce the retry storm that follows.
//
// One second is the floor that is always honest: the bucket refills at
// RATE_LIMIT_RPS tokens per second, so at any rate of 1 rps or more a token
// exists by the time a client that waited this long comes back. A store that
// refills slower than that states its own interval through
// RateLimitKeyedRetryAfter, as the sign-in limiter does. A RATE_LIMIT_RPS below
// 1 is the one configuration still sent this hint, because RateLimit is handed
// the store and not the rate it was built with.
const retryAfterSeconds = "1"

// RateLimit returns middleware that enforces per-IP token-bucket rate limiting.
//
// The rate-limit bucket is keyed on the host portion of r.RemoteAddr only (no
// port). r.RemoteAddr is expected to have already been resolved to the real
// client IP by RealIPMiddleware, which honors X-Forwarded-For and X-Real-IP
// only when the direct TCP peer is within a trusted-proxy CIDR. That
// middleware must be installed before this one in the chain.
func RateLimit(store *ratelimit.Store) func(http.Handler) http.Handler {
	return RateLimitKeyed(store, "ip")
}

// RateLimitKeyed is RateLimit with the metrics label named by the caller.
//
// A route with its own store needs its own label, or its rejections land in the
// same counter as general traffic and there is no way to tell a burst of failed
// sign-ins from a busy gateway — which is the one thing the counter is worth
// watching for.
func RateLimitKeyed(store *ratelimit.Store, rejectionLabel string) func(http.Handler) http.Handler {
	return rateLimitKeyed(store, rejectionLabel, retryAfterSeconds)
}

// RateLimitKeyedRetryAfter is RateLimitKeyed for a store that refills slower
// than one token a second. refill is the interval at which the store's bucket
// gains a token, and the Retry-After a rejection carries is that interval
// rounded up to whole seconds.
//
// The fixed one-second hint is only honest at a rate of 1 rps or more. Below
// it a client that waited as long as it was told came back before a token had
// returned and was shed again, so every limiter slower than that names its own
// interval here rather than inheriting a hint that describes another one.
func RateLimitKeyedRetryAfter(store *ratelimit.Store, rejectionLabel string, refill time.Duration) func(http.Handler) http.Handler {
	seconds := max(1, int64(math.Ceil(refill.Seconds())))
	return rateLimitKeyed(store, rejectionLabel, strconv.FormatInt(seconds, 10))
}

func rateLimitKeyed(store *ratelimit.Store, rejectionLabel, retryAfter string) func(http.Handler) http.Handler {
	if store == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Extract the host without the port so ephemeral port variation
			// does not produce separate buckets for the same client.
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				// r.RemoteAddr is already a bare IP (set by RealIPMiddleware).
				host = r.RemoteAddr
			}
			if !store.Allow(host) {
				metrics.RateLimitRejections.WithLabelValues(rejectionLabel).Inc()
				// Set before WriteOpenAI: it writes the status line, after
				// which the header map is no longer sent.
				w.Header().Set("Retry-After", retryAfter)
				apierror.WriteOpenAI(w, http.StatusTooManyRequests,
					"rate limit exceeded", "rate_limit_error", "rate_limit_exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
