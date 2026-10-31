package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/pkg/circuitbreaker"
)

// bodyUpstream records every body it receives in full and answers with a
// Responses object carrying usage.
type bodyUpstream struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newBodyUpstream(t *testing.T) *bodyUpstream {
	t.Helper()
	up := &bodyUpstream{}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		up.mu.Lock()
		up.bodies = append(up.bodies, string(b))
		up.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","usage":{"input_tokens":12,"output_tokens":4,"total_tokens":16}}`))
	}))
	t.Cleanup(up.Close)
	return up
}

func (u *bodyUpstream) received() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.bodies...)
}

func largeBodyGateway(t *testing.T, upstream string, maxRequestBytes int64, plugins ...config.PluginConfig) (*aigateway.Gateway, *recordingLogStore) {
	t.Helper()
	t.Setenv("FERRO_MODEL_CATALOG_TIMEOUT", "0")
	store := &recordingLogStore{}
	gw, err := aigateway.New(config.Config{
		Strategy:        config.StrategyConfig{Mode: config.ModeSingle},
		Targets:         []config.Target{{VirtualKey: "stub", Models: []string{"stub-model"}}},
		MaxRequestBytes: maxRequestBytes,
		Plugins:         append(requestLoggerConfig(), plugins...),
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

// largeResponsesBody is a Responses create body of at least size bytes whose
// model and ceiling follow its input, the order openai-python writes them in.
func largeResponsesBody(size int, ceiling int) string {
	return `{"input":"` + strings.Repeat("x", size) + `","max_output_tokens":` + strconv.Itoa(ceiling) + `,"model":"stub-model"}`
}

func postLargeResponses(t *testing.T, gw *aigateway.Gateway, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	ResponsesCreate(gw)(w, r)
	return w
}

// A Responses body larger than the guardrail projection's 1 MiB — an image or
// a file inlined as base64, a long conversation — had its model and its
// ceiling read from nothing: the peek gave up past that cap. Placed by its
// model, such a body was answered 400 "model is required" though it named one;
// placed by X-Provider, it reached the max-token guardrail with no ceiling,
// which it approves, and was forwarded carrying the ceiling it never read.
func TestResponsesCreate_ReadsGovernedFieldsPastTheProjectionCap(t *testing.T) {
	const size = projectionCap + 512<<10

	t.Run("routed-by-model", func(t *testing.T) {
		up := newBodyUpstream(t)
		gw, store := largeBodyGateway(t, up.URL, 0)
		body := largeResponsesBody(size, 64)

		w := postLargeResponses(t, gw, body, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %.300s", w.Code, w.Body.String())
		}
		if got := up.received(); len(got) != 1 || got[0] != body {
			t.Errorf("upstream received %d bodies, want the one body forwarded intact", len(got))
		}
		var recorded bool
		for _, e := range store.all() {
			if e.Provider == "stub" && e.Model == "stub-model" {
				recorded = true
			}
		}
		if !recorded {
			t.Errorf("no request-log row names the target and model that served the request")
		}
	})

	for name, headers := range map[string]map[string]string{
		"by-model":   nil,
		"x-provider": {"X-Provider": "stub"},
	} {
		t.Run("ceiling-governed-"+name, func(t *testing.T) {
			up := newBodyUpstream(t)
			gw, _ := largeBodyGateway(t, up.URL, 0, maxTokenConfig(0))

			w := postLargeResponses(t, gw, largeResponsesBody(size, 100000), headers)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "request_rejected") {
				t.Errorf("status = %d, want 400 request_rejected from the max-token guardrail; body: %.300s", w.Code, w.Body.String())
			}
			if got := up.received(); len(got) != 0 {
				t.Errorf("upstream received %d requests, want 0: the ceiling exceeds the guardrail's cap", len(got))
			}
		})
	}
}

// A body past max_request_bytes is answered 413 before anything is forwarded,
// as a body past the cap is on every other data-plane route: the gateway
// cannot read the fields it routes and governs on from a body it will not hold.
// Placed by X-Provider it is still recorded as the target's failed request,
// and kept off the breaker, as a body that runs past the cap mid-forward is.
func TestResponsesCreate_RefusesABodyPastTheRequestBodyCap(t *testing.T) {
	const limit = projectionCap + 256<<10

	for name, headers := range map[string]map[string]string{
		"by-model":   nil,
		"x-provider": {"X-Provider": "stub"},
	} {
		t.Run(name, func(t *testing.T) {
			up := newBodyUpstream(t)
			gw, store := largeBodyGateway(t, up.URL, limit)

			w := postLargeResponses(t, gw, largeResponsesBody(limit, 64), headers)
			if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "request_too_large") {
				t.Errorf("status = %d, want 413 request_too_large; body: %.300s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "model is required") {
				t.Errorf("answer asks for the model the body carried: %.300s", w.Body.String())
			}
			if got := up.received(); len(got) != 0 {
				t.Errorf("upstream received %d requests, want 0", len(got))
			}
			if headers != nil {
				if got := onErrorRows(store); got != 1 {
					t.Errorf("on_error rows naming the target = %d, want 1 (the request failed); rows: %+v", got, store.all())
				}
				if state := gw.CircuitBreakerStates()["stub"]; state != float64(circuitbreaker.StateClosed) {
					t.Errorf("breaker state = %v, want closed: the caller's body size is not the target's health", state)
				}
			}
		})
	}
}
