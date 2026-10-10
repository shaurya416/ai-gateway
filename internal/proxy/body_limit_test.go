package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/pkg/circuitbreaker"
)

// A body the caller sent past the gateway's own request-body limit is the
// caller's mistake, answered 413 by the gateway before the upstream ever saw a
// whole request. The forward reported it as a failure to reach the upstream,
// so the shared breaker scored it against the target: with the default
// failure threshold, a handful of oversized uploads from one caller opened the
// circuit and refused every caller's chat, embeddings and pass-through traffic
// to a healthy provider. It must still be recorded as a failed request.
func TestForward_OversizedBodyDoesNotTripTheBreaker(t *testing.T) {
	for name := range abortSurfaces(nil) {
		t.Run(name, func(t *testing.T) {
			var completed atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Read the whole body before answering, so the forward's
				// outcome is decided by the request write, never by an early
				// response.
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					return
				}
				completed.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			t.Cleanup(up.Close)
			gw, store := abortTestGateway(t, up.URL)
			h := abortSurfaces(gw)[name]

			if w := postOversized(t, h); w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("oversized body: status = %d, want 413; body: %s", w.Code, w.Body.String())
			}
			if state := gw.CircuitBreakerStates()["stub"]; state != float64(circuitbreaker.StateClosed) {
				t.Errorf("breaker state = %v after an oversized request body, want closed (%v): the caller's body size is not the target's health", state, float64(circuitbreaker.StateClosed))
			}
			if got := onErrorRows(store); got != 1 {
				t.Errorf("on_error rows naming the target = %d, want 1 (the request failed); rows: %+v", got, store.all())
			}

			// The next caller's ordinary request still reaches the target.
			if w := postResponses(t, h, nil); w.Code != http.StatusOK {
				t.Errorf("follow-up request: status = %d, want 200; body: %s", w.Code, w.Body.String())
			}
			if got := completed.Load(); got != 1 {
				t.Errorf("upstream completed %d requests, want 1 (the follow-up)", got)
			}
		})
	}
}

// An oversized body is no answer from the upstream, so it is not scored as one
// in either direction. Scored as a success, it reset the failure count of a
// target that was failing — an oversized upload every few requests kept a
// broken upstream's circuit from ever opening — and could close a half-open
// circuit on an upstream that had answered nothing.
func TestForward_OversizedBodyDoesNotResetTheFailureCount(t *testing.T) {
	for name := range abortSurfaces(nil) {
		t.Run(name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					return
				}
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(up.Close)
			t.Setenv("FERRO_MODEL_CATALOG_TIMEOUT", "0")
			gw, err := aigateway.New(config.Config{
				Strategy: config.StrategyConfig{Mode: config.ModeSingle},
				Targets: []config.Target{{
					VirtualKey:     "stub",
					Models:         []string{"stub-model"},
					CircuitBreaker: &config.CircuitBreakerConfig{FailureThreshold: 2, Timeout: "30s"},
				}},
			})
			if err != nil {
				t.Fatalf("aigateway.New: %v", err)
			}
			t.Cleanup(func() { _ = gw.Close() })
			gw.RegisterProvider(&proxiableStub{name: "stub", baseURL: up.URL, models: []string{"stub-model"}})
			h := abortSurfaces(gw)[name]

			if w := postResponses(t, h, nil); w.Code != http.StatusInternalServerError {
				t.Fatalf("first failure: status = %d, want 500; body: %s", w.Code, w.Body.String())
			}
			if w := postOversized(t, h); w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("oversized body: status = %d, want 413; body: %s", w.Code, w.Body.String())
			}
			if w := postResponses(t, h, nil); w.Code != http.StatusInternalServerError {
				t.Fatalf("second failure: status = %d, want 500; body: %s", w.Code, w.Body.String())
			}
			if state := gw.CircuitBreakerStates()["stub"]; state != float64(circuitbreaker.StateOpen) {
				t.Errorf("breaker state = %v after two upstream 500s with an oversized body between them, want open (%v): the oversized body reset the failure count", state, float64(circuitbreaker.StateOpen))
			}
		})
	}
}

// postOversized posts a body past the request-body limit through h, wrapped as
// the shared body-limit middleware wraps every /v1/* request. The limit sits
// above the projection cap, as the shared one does, so the body is accepted by
// every pre-forward read and only runs out mid-forward.
func postOversized(t *testing.T, h http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	const limit = projectionCap + 64<<10
	oversized := `{"model":"stub-model","input":"` + strings.Repeat("x", limit) + `"}`
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(oversized))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Provider", "stub")
	w := httptest.NewRecorder()
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	h(w, r)
	return w
}

// A request body the caller cannot deliver — chunked framing that breaks off
// mid-upload — fails while the forward is streaming it upstream, and the
// forward reported that as a failure to reach the provider. The caller was
// answered 502 "upstream connection failed" for its own malformed request, and
// the shared breaker scored it against the target, so a handful of such
// requests from one caller opened the circuit and refused every caller's
// chat, embeddings and pass-through traffic to a healthy provider. net/http
// ends the request as the caller's when the caller's connection breaks
// mid-body; a body that breaks over a live connection is no different.
func TestForward_UnreadableRequestBodyIsTheCallers(t *testing.T) {
	for name := range abortSurfaces(nil) {
		t.Run(name, func(t *testing.T) {
			var completed atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					return
				}
				completed.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			t.Cleanup(up.Close)
			gw, store := abortTestGateway(t, up.URL)
			h := abortSurfaces(gw)[name]
			srv, done := serveUntilDone(t, h)

			status, body := postBrokenChunked(t, srv.URL, "/v1/responses")
			waitDone(t, done)

			if status != http.StatusBadRequest {
				t.Errorf("unreadable body: status = %d, want 400; body: %s", status, body)
			}
			if state := gw.CircuitBreakerStates()["stub"]; state != float64(circuitbreaker.StateClosed) {
				t.Errorf("breaker state = %v after a request body the caller broke, want closed (%v): the caller's framing is not the target's health", state, float64(circuitbreaker.StateClosed))
			}
			if got := onErrorRows(store); got != 1 {
				t.Errorf("on_error rows naming the target = %d, want 1 (the request failed); rows: %+v", got, store.all())
			}

			// The next caller's ordinary request still reaches the target.
			if w := postResponses(t, h, nil); w.Code != http.StatusOK {
				t.Errorf("follow-up request: status = %d, want 200; body: %s", w.Code, w.Body.String())
			}
			if got := completed.Load(); got != 1 {
				t.Errorf("upstream completed %d requests, want 1 (the follow-up)", got)
			}
		})
	}
}

// The Files and Batch forward is ungoverned, so there is no breaker to keep
// clear, but its caller was told the same thing: an upload whose framing broke
// was answered 502 "upstream connection failed".
func TestBatchHandler_UnreadableRequestBodyIsTheCallers(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"file-abc","object":"file"}`))
	}))
	t.Cleanup(up.Close)
	src := &batchSourceStub{target: "openai", provider: &batchStub{name: "openai", baseURL: up.URL}}
	srv, done := serveUntilDone(t, BatchHandler(src))

	status, body := postBrokenChunked(t, srv.URL, "/v1/files")
	waitDone(t, done)

	if status != http.StatusBadRequest {
		t.Errorf("unreadable upload: status = %d, want 400; body: %s", status, body)
	}
}

// POST /v1/responses watches its body under the lifecycle's context, and the
// fixed-target forward it calls watches the same request again. A failure
// cancels that context first, and an HTTP/2 transport answers a cancelled
// context by closing the request body it holds, from another goroutine, which
// can run before the forward's own read has returned the failure. A watcher
// closed first ignores what it reads next, so the forward found no body failure
// and answered the caller's own body 502 "upstream connection failed". Here the
// transport's close runs at the worst moment, deterministically.
func TestWatchCallerBody_SecondWatchSeesTheFailure(t *testing.T) {
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", iotest.ErrReader(io.ErrUnexpectedEOF))
	var forward *callerBody
	watchCallerBody(r, func() { _ = forward.Close() })
	fr := r.WithContext(t.Context())
	forward = watchCallerBody(fr, nil)

	if _, err := io.ReadAll(fr.Body); err == nil {
		t.Fatal("reading a broken body succeeded")
	}
	if err := forward.failure(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("forward's body failure = %v, want %v", err, io.ErrUnexpectedEOF)
	}
}

// postBrokenChunked sends a chunked request whose framing breaks after the
// first chunk, over a connection that stays open, and returns the status and
// body the gateway answered. The chunk runs past the projection cap, so the
// body is still being streamed upstream when the framing breaks.
func postBrokenChunked(t *testing.T, url, path string) (int, string) {
	t.Helper()
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	chunk := `{"model":"stub-model","input":"` + strings.Repeat("x", projectionCap+64<<10)
	if _, err := fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: gateway\r\nContent-Type: application/json\r\nX-Provider: stub\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\nnot-a-chunk-size\r\n", path, len(chunk), chunk); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}
