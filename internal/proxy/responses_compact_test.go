package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/config"
)

// pathUpstream records the path of every request it receives and answers with
// a compacted response carrying usage, the shape POST /responses/compact
// returns.
type pathUpstream struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
}

func newPathUpstream(t *testing.T) *pathUpstream {
	t.Helper()
	up := &pathUpstream{}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		up.mu.Lock()
		up.paths = append(up.paths, r.Method+" "+r.URL.Path)
		up.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmp_1","object":"response.compaction","created_at":1,"output":[],"usage":{"input_tokens":12,"output_tokens":4,"total_tokens":16}}`))
	}))
	t.Cleanup(up.Close)
	return up
}

func (u *pathUpstream) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.paths...)
}

// compactGateway serves stub-model from the "stub" target and pins the
// Responses id sub-routes to a second target, "pinned", which does not serve
// it. responsesTarget is left empty to leave the id sub-routes unconfigured.
func compactGateway(t *testing.T, stub, pinned, responsesTarget string) (*aigateway.Gateway, *recordingLogStore) {
	t.Helper()
	t.Setenv("FERRO_MODEL_CATALOG_TIMEOUT", "0")
	store := &recordingLogStore{}
	gw, err := aigateway.New(config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets: []config.Target{
			{VirtualKey: "stub", Models: []string{"stub-model"}},
			{VirtualKey: "pinned", Models: []string{"pinned-model"}},
		},
		ResponsesTarget: responsesTarget,
		Plugins:         append(requestLoggerConfig(), wordFilterConfig("forbidden")),
	})
	if err != nil {
		t.Fatalf("aigateway.New: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })
	gw.RegisterProvider(&proxiableStub{name: "stub", baseURL: stub, models: []string{"stub-model"}})
	gw.RegisterProvider(&proxiableStub{name: "pinned", baseURL: pinned, models: []string{"pinned-model"}})
	gw.SetRequestLogWriter(store)
	if err := gw.LoadPlugins(); err != nil {
		t.Fatalf("LoadPlugins: %v", err)
	}
	return gw, store
}

// POST /v1/responses/compact runs a model over the caller's conversation and
// bills for it — it names a model and returns usage, as create does — but it
// sits under /v1/responses/, where the id sub-routes are mounted. Those
// forwarded it as one: with responses_target set it went, ungoverned, to that
// target whatever model it named, past every guardrail, budget and rate limit
// and absent from the request log; with responses_target unset it was a 501.
func TestResponsesCompact_IsRoutedAndGovernedAsCreate(t *testing.T) {
	compact := func(t *testing.T, gw *aigateway.Gateway, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		ResponsesIDs(gw)(w, r)
		return w
	}

	t.Run("routed-by-model", func(t *testing.T) {
		stub, pinned := newPathUpstream(t), newPathUpstream(t)
		gw, store := compactGateway(t, stub.URL, pinned.URL, "pinned")

		w := compact(t, gw, "/v1/responses/compact", `{"model":"stub-model","input":"summarize this conversation"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		if got := stub.seen(); len(got) != 1 || got[0] != "POST /responses/compact" {
			t.Errorf("stub-model's target received %q, want one POST /responses/compact", got)
		}
		if got := pinned.seen(); len(got) != 0 {
			t.Errorf("responses_target received %q, want nothing: it does not serve stub-model", got)
		}
		var recorded bool
		for _, e := range store.all() {
			if e.Provider == "stub" && e.Model == "stub-model" {
				recorded = true
			}
		}
		if !recorded {
			t.Errorf("no request-log row names the target and model that served the compaction; rows: %+v", store.all())
		}
	})

	t.Run("governed", func(t *testing.T) {
		stub, pinned := newPathUpstream(t), newPathUpstream(t)
		gw, _ := compactGateway(t, stub.URL, pinned.URL, "pinned")

		w := compact(t, gw, "/v1/responses/compact", `{"model":"stub-model","input":"the forbidden thing"}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 from the content guardrail; body: %s", w.Code, w.Body.String())
		}
		if got := append(stub.seen(), pinned.seen()...); len(got) != 0 {
			t.Errorf("upstreams received %q, want nothing: the guardrail blocks this body on create", got)
		}
	})

	// The path is judged as the upstream will read it, so a spelling that
	// cleans to the same operation is not an id sub-route either.
	t.Run("uncleaned-path", func(t *testing.T) {
		stub, pinned := newPathUpstream(t), newPathUpstream(t)
		gw, _ := compactGateway(t, stub.URL, pinned.URL, "pinned")

		w := compact(t, gw, "/v1/responses//compact/", `{"model":"stub-model","input":"the forbidden thing"}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 from the content guardrail; body: %s", w.Code, w.Body.String())
		}
		if got := append(stub.seen(), pinned.seen()...); len(got) != 0 {
			t.Errorf("upstreams received %q, want nothing", got)
		}
	})

	t.Run("without-responses-target", func(t *testing.T) {
		stub, pinned := newPathUpstream(t), newPathUpstream(t)
		gw, _ := compactGateway(t, stub.URL, pinned.URL, "")

		w := compact(t, gw, "/v1/responses/compact", `{"model":"stub-model","input":"summarize this conversation"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: compaction needs no responses_target; body: %s", w.Code, w.Body.String())
		}
		if got := stub.seen(); len(got) != 1 {
			t.Errorf("stub-model's target received %q, want one request", got)
		}
	})

	// The id sub-routes themselves still pin to responses_target.
	t.Run("id-routes-unchanged", func(t *testing.T) {
		stub, pinned := newPathUpstream(t), newPathUpstream(t)
		gw, _ := compactGateway(t, stub.URL, pinned.URL, "pinned")

		for _, rt := range []struct{ method, path string }{
			{http.MethodGet, "/v1/responses/resp_1"},
			{http.MethodPost, "/v1/responses/resp_1/cancel"},
		} {
			r := httptest.NewRequestWithContext(t.Context(), rt.method, rt.path, nil)
			w := httptest.NewRecorder()
			ResponsesIDs(gw)(w, r)
			if w.Code != http.StatusOK {
				t.Errorf("%s %s: status = %d, want 200", rt.method, rt.path, w.Code)
			}
		}
		if got := pinned.seen(); len(got) != 2 {
			t.Errorf("responses_target received %q, want both id sub-route requests", got)
		}
		if got := stub.seen(); len(got) != 0 {
			t.Errorf("stub target received %q, want nothing", got)
		}
	})
}
