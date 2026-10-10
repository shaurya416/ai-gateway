package proxy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/streamio"
	"github.com/ferro-labs/ai-gateway/pkg/circuitbreaker"
)

// abortSurfaces are the two governed forwards: the generic /v1/* pass-through
// and the priced POST /v1/responses.
func abortSurfaces(gw *aigateway.Gateway) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"passthrough": Handler(gw),
		"responses":   ResponsesCreate(gw),
	}
}

// streamThenHold answers with one SSE event, flushed, and then holds the
// response open until the request ends — a stream in progress.
func streamThenHold(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"))
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// streamThenDrop answers with one SSE event, flushed, and then drops the
// connection without ending the response — an upstream that died mid-stream.
func streamThenDrop(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"))
		_ = http.NewResponseController(w).Flush()
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// abortTestGateway is a single-target gateway with a breaker that opens on the
// first counted failure, and a request log, so one request shows both what
// the breaker scored and whether the request was recorded at all.
func abortTestGateway(t *testing.T, upstream string) (*aigateway.Gateway, *recordingLogStore) {
	t.Helper()
	t.Setenv("FERRO_MODEL_CATALOG_TIMEOUT", "0")
	store := &recordingLogStore{}
	gw, err := aigateway.New(config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets: []config.Target{{
			VirtualKey:     "stub",
			Models:         []string{"stub-model"},
			CircuitBreaker: &config.CircuitBreakerConfig{FailureThreshold: 1, Timeout: "30s"},
		}},
		Plugins: requestLoggerConfig(),
	})
	if err != nil {
		t.Fatalf("aigateway.New: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })
	gw.RegisterProvider(&proxiableStub{name: "stub", baseURL: upstream, models: []string{"stub-model"}})
	gw.SetRequestLogWriter(store)
	if err := gw.LoadPlugins(); err != nil {
		t.Fatalf("LoadPlugins: %v", err)
	}
	return gw, store
}

// serveUntilDone serves h on a real HTTP server — the reverse proxy only
// aborts a broken body copy under one — and returns a channel closed once the
// handler has returned or unwound.
func serveUntilDone(t *testing.T, h http.HandlerFunc) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, done
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not finish")
	}
}

func postStream(ctx context.Context, t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/responses",
		strings.NewReader(`{"model":"stub-model","input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return resp
}

func onErrorRows(store *recordingLogStore) int {
	n := 0
	for _, e := range store.all() {
		if e.Stage == "on_error" && e.Provider == "stub" && e.ErrorMessage != "" {
			n++
		}
	}
	return n
}

// A caller that closes a stream it no longer wants — a user pressing stop — is
// the caller's cancellation, which the breaker excludes. The reverse proxy
// reports a broken body copy by panicking, and that panic reached the breaker
// as a failure whatever caused it, so a single disconnect opened the circuit
// and the target's next request on every surface was refused with a 503.
func TestForward_ClientDisconnectMidStreamDoesNotTripBreaker(t *testing.T) {
	for name := range abortSurfaces(nil) {
		t.Run(name, func(t *testing.T) {
			up := streamThenHold(t)
			gw, store := abortTestGateway(t, up.URL)
			srv, done := serveUntilDone(t, abortSurfaces(gw)[name])

			ctx, cancel := context.WithCancel(t.Context())
			resp := postStream(ctx, t, srv.URL)
			if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
				t.Fatalf("first event: %v", err)
			}
			cancel()
			_ = resp.Body.Close()
			waitDone(t, done)

			if state := gw.CircuitBreakerStates()["stub"]; state != float64(circuitbreaker.StateClosed) {
				t.Errorf("breaker state = %v after a client disconnect, want closed (%v)", state, float64(circuitbreaker.StateClosed))
			}
			// The request is still recorded, as a failure: it did not complete.
			if got := onErrorRows(store); got != 1 {
				t.Errorf("on_error rows naming the target = %d, want 1; rows: %+v", got, store.all())
			}
		})
	}
}

// An upstream that dies mid-stream, or stalls past the stream idle bound, IS a
// target failure, and the request must be recorded as one. Before, the abort
// panic skipped the lifecycle: the breaker scored it, but no on_error stage
// ran, so the request left no request-log row and no error metric. The client
// must still see the response break off rather than end cleanly.
func TestForward_UpstreamFailureMidStreamIsRecorded(t *testing.T) {
	upstreams := map[string]func(*testing.T) *httptest.Server{
		"upstream drops":     streamThenDrop,
		"idle bound elapses": streamThenHold,
	}
	for upName, newUpstream := range upstreams {
		for name := range abortSurfaces(nil) {
			t.Run(upName+"/"+name, func(t *testing.T) {
				defer streamio.SetIdleTimeoutForTest(100 * time.Millisecond)()
				up := newUpstream(t)
				gw, store := abortTestGateway(t, up.URL)
				srv, done := serveUntilDone(t, abortSurfaces(gw)[name])

				resp := postStream(t.Context(), t, srv.URL)
				_, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				waitDone(t, done)

				if readErr == nil {
					t.Error("client read the truncated stream to a clean end; want the response to break off")
				}
				if state := gw.CircuitBreakerStates()["stub"]; state != float64(circuitbreaker.StateOpen) {
					t.Errorf("breaker state = %v, want open (%v): a dead or stalled upstream is a target failure", state, float64(circuitbreaker.StateOpen))
				}
				if got := onErrorRows(store); got != 1 {
					t.Errorf("on_error rows naming the target = %d, want 1; rows: %+v", got, store.all())
				}
				// A stall is recorded as the idle bound, not as a cancellation
				// that reads like the caller's own.
				if upName == "idle bound elapses" {
					for _, e := range store.all() {
						if e.Stage == "on_error" && !strings.Contains(e.ErrorMessage, streamio.ErrIdleTimeout.Error()) {
							t.Errorf("on_error error = %q, want it to name the idle bound (%q)", e.ErrorMessage, streamio.ErrIdleTimeout.Error())
						}
					}
				}
			})
		}
	}
}
