package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
	"github.com/ferro-labs/ai-gateway/providers"
)

// retryAfterStub is the smallest provider NewRouter will build a gateway over.
type retryAfterStub struct{}

func (retryAfterStub) Name() string                { return "stub" }
func (retryAfterStub) SupportsModel(m string) bool { return m == "stub-model" }
func (retryAfterStub) Complete(context.Context, providers.Request) (*providers.Response, error) {
	return &providers.Response{Model: "stub-model"}, nil
}

// TestSessionEndpoint_RetryAfterIsWhenATokenReturns holds the Retry-After a
// throttled sign-in carries to the limiter that throttled it: a client that
// waits exactly as long as it was told must be admitted.
//
// The sign-in limiter refills ten attempts a minute, one every six seconds, and
// the hint was the general limiter's fixed one second. A client honouring it came
// back five seconds early and was shed again — and again, for as long as it kept
// believing the header — so the one thing a 429 exists to tell the caller was
// wrong on the one route where waiting is the whole remedy.
func TestSessionEndpoint_RetryAfterIsWhenATokenReturns(t *testing.T) {
	reg := providers.NewRegistry()
	reg.Register(retryAfterStub{})
	router := NewRouter(reg, repository.NewKeyStore(), repository.NewSessionStore(), nil, nil, nil, nil, nil, nil, nil, "", nil)

	// Exhaust one address against the production wiring and read the hint.
	var retryAfter string
	for attempt := 1; attempt <= 200 && retryAfter == ""; attempt++ {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/session", nil)
		req.Header.Set("Authorization", fmt.Sprintf("Bearer wrong-key-%d", attempt))
		req.RemoteAddr = "192.0.2.30:54321"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			retryAfter = w.Header().Get("Retry-After")
			if retryAfter == "" {
				t.Fatal("a throttled sign-in carried no Retry-After")
			}
		}
	}
	if retryAfter == "" {
		t.Fatal("200 sign-in attempts from one address were never throttled")
	}
	seconds, err := strconv.Atoi(retryAfter)
	if err != nil || seconds < 1 {
		t.Fatalf("Retry-After = %q, want a positive whole number of seconds", retryAfter)
	}

	// Replay the same exhaustion on the limiter that route is built with, on a
	// clock the test controls, and wait exactly as long as the hint said.
	now := time.Unix(1_700_000_000, 0)
	limiter := newSessionLimiter()
	limiter.SetNowForTest(func() time.Time { return now })
	const addr = "192.0.2.31"
	drained := false
	for range 200 {
		if !limiter.Allow(addr) {
			drained = true
			break
		}
	}
	if !drained {
		t.Fatal("could not exhaust the sign-in limiter")
	}
	now = now.Add(time.Duration(seconds) * time.Second)
	if !limiter.Allow(addr) {
		t.Fatalf("a sign-in retried after Retry-After: %s was refused again: the hint is earlier than the limiter's next token", retryAfter)
	}
}
