package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/pkg/circuitbreaker"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// answeringWith is an upstream that answers every request with status and an
// OpenAI-shaped error body, plus any extra headers.
func answeringWith(t *testing.T, status int, headers map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"message":"rejected upstream","type":"invalid_request_error"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func postResponses(t *testing.T, h http.HandlerFunc, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"stub-model","input":"hi"}`))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

// An upstream that refuses a forwarded request — a 401 from a revoked provider
// credential — failed the request, and the caller received that failure. The
// forward reported nothing for any status below 500, so the lifecycle ran the
// after_request stage and the request log, metrics and completed event all
// recorded a success. The refusal is still kept off the shared breaker: it is
// the upstream's verdict on the caller's request, not on the target's health.
func TestForward_UpstreamClientErrorIsRecordedAsFailure(t *testing.T) {
	for name := range abortSurfaces(nil) {
		t.Run(name, func(t *testing.T) {
			up := answeringWith(t, http.StatusUnauthorized, nil)
			gw, store := abortTestGateway(t, up.URL)

			w := postResponses(t, abortSurfaces(gw)[name], nil)

			if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "rejected upstream") {
				t.Fatalf("client got %d %q, want the upstream's own 401 relayed", w.Code, w.Body.String())
			}
			if got := onErrorRows(store); got != 1 {
				t.Errorf("on_error rows naming the target = %d, want 1; rows: %+v", got, store.all())
			}
			for _, e := range store.all() {
				if e.Stage == "after_request" {
					t.Errorf("request log has a success row for a request the upstream refused: %+v", e)
				}
			}
			if state := gw.CircuitBreakerStates()["stub"]; state != float64(circuitbreaker.StateClosed) {
				t.Errorf("breaker state = %v, want closed (%v): an upstream 4xx must not trip the shared breaker", state, float64(circuitbreaker.StateClosed))
			}
		})
	}
}

// chatStub is a proxiable stub that also serves chat, so a test can see which
// target routing chose after a forward.
type chatStub struct {
	proxiableStub
	calls atomic.Int32
}

func (p *chatStub) Complete(context.Context, core.Request) (*core.Response, error) {
	p.calls.Add(1)
	return &core.Response{ID: "served-by-" + p.name, Model: "stub-model"}, nil
}

// A 429 parks the target for its Retry-After on every routed surface, so the
// next request is offered to a sibling instead of paying another 429. A 429
// relayed by a forward reported nothing, so the throttled target kept its full
// share of traffic.
func TestForward_UpstreamRateLimitParksTheTarget(t *testing.T) {
	for name := range abortSurfaces(nil) {
		t.Run(name, func(t *testing.T) {
			up := answeringWith(t, http.StatusTooManyRequests, map[string]string{"Retry-After": "30"})
			t.Setenv("FERRO_MODEL_CATALOG_TIMEOUT", "0")
			gw, err := aigateway.New(config.Config{
				Strategy: config.StrategyConfig{Mode: config.ModeFallback},
				Targets: []config.Target{
					{VirtualKey: "stub", Models: []string{"stub-model"}},
					{VirtualKey: "spare", Models: []string{"stub-model"}},
				},
			})
			if err != nil {
				t.Fatalf("aigateway.New: %v", err)
			}
			t.Cleanup(func() { _ = gw.Close() })
			throttled := &chatStub{proxiableStub: proxiableStub{name: "stub", baseURL: up.URL, models: []string{"stub-model"}}}
			spare := &chatStub{proxiableStub: proxiableStub{name: "spare", baseURL: up.URL, models: []string{"stub-model"}}}
			gw.RegisterProvider(throttled)
			gw.RegisterProvider(spare)

			if w := postResponses(t, abortSurfaces(gw)[name], map[string]string{"X-Provider": "stub"}); w.Code != http.StatusTooManyRequests {
				t.Fatalf("forward: status = %d, want the upstream's 429 relayed", w.Code)
			}

			resp, err := gw.Route(t.Context(), core.Request{
				Model:    "stub-model",
				Messages: []core.Message{{Role: "user", Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if resp.ID != "served-by-spare" || throttled.calls.Load() != 0 {
				t.Errorf("chat served by %q with %d calls to the throttled target; want the sibling while the target is parked", resp.ID, throttled.calls.Load())
			}
		})
	}
}
