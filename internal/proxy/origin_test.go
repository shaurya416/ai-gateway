package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/providers"
)

// corsUpstream answers the way an upstream that applies a CORS policy of its
// own does when a request carries an Origin. "refuse" is gin-contrib/cors as
// Ollama configures it: an Origin outside OLLAMA_ORIGINS is answered 403.
// "allow-all" is Starlette's CORSMiddleware with allow_origins=["*"], which
// adds its own Access-Control-Allow-Origin. A request without an Origin is not
// a CORS request to either, and is served.
func corsUpstream(t *testing.T, policy string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("Origin") != "" {
			if policy == "refuse" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A browser sends its Origin to the gateway, whose CORS layer decides whether
// that site may call it. The forwards relayed the header upstream, so the
// upstream decided again under its own policy: an Ollama target refused every
// forward from a browser application the gateway admits with a 403, while chat
// to the same target, which never sends the header, was served; and an
// upstream that allows every origin added its own Access-Control-Allow-Origin
// beside the gateway's, a pair a browser refuses as a CORS failure.
func TestForwardsDoNotCarryTheBrowserOriginUpstream(t *testing.T) {
	const origin = "https://app.example.com"

	for _, policy := range []string{"refuse", "allow-all"} {
		t.Run(policy, func(t *testing.T) {
			up := corsUpstream(t, policy)
			t.Setenv("FERRO_MODEL_CATALOG_TIMEOUT", "0")
			gw, err := aigateway.New(config.Config{
				Strategy:        config.StrategyConfig{Mode: config.ModeSingle},
				Targets:         []config.Target{{VirtualKey: "stub", Models: []string{"stub-model"}}},
				ResponsesTarget: "stub",
			})
			if err != nil {
				t.Fatalf("aigateway.New: %v", err)
			}
			t.Cleanup(func() { _ = gw.Close() })
			gw.RegisterProvider(&proxiableStub{name: "stub", baseURL: up.URL, models: []string{"stub-model"}})
			reg := providers.NewRegistry()
			reg.Register(&proxiableStub{name: "stub", baseURL: up.URL, models: []string{"stub-model"}})

			surfaces := map[string]struct {
				h      http.HandlerFunc
				method string
				path   string
			}{
				"passthrough":            {h: Handler(gw), method: http.MethodPost, path: "/v1/fine_tuning/jobs"},
				"passthrough-ungoverned": {h: Handler(reg), method: http.MethodPost, path: "/v1/fine_tuning/jobs"},
				"responses":              {h: ResponsesCreate(gw), method: http.MethodPost, path: "/v1/responses"},
				"responses-ids":          {h: ResponsesIDs(gw), method: http.MethodGet, path: "/v1/responses/resp_1"},
				"files": {
					h:      BatchHandler(&batchSourceStub{target: "openai", provider: &batchStub{name: "openai", baseURL: up.URL}}),
					method: http.MethodGet,
					path:   "/v1/files",
				},
			}
			for name, s := range surfaces {
				t.Run(name, func(t *testing.T) {
					// What the gateway's CORS middleware sets for an origin on
					// CORS_ORIGINS before the handler runs.
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Access-Control-Allow-Origin", origin)
						s.h(w, r)
					}))
					t.Cleanup(srv.Close)

					var body io.Reader
					if s.method == http.MethodPost {
						body = strings.NewReader(`{"model":"stub-model","input":"hi"}`)
					}
					req, err := http.NewRequestWithContext(t.Context(), s.method, srv.URL+s.path, body)
					if err != nil {
						t.Fatalf("new request: %v", err)
					}
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("X-Provider", "stub")
					req.Header.Set("Origin", origin)
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatalf("request: %v", err)
					}
					defer func() { _ = resp.Body.Close() }()
					_, _ = io.Copy(io.Discard, resp.Body)

					if resp.StatusCode != http.StatusOK {
						t.Errorf("status = %d, want 200: the upstream applied its own CORS policy to a request the gateway admitted", resp.StatusCode)
					}
					if got := resp.Header.Values("Access-Control-Allow-Origin"); len(got) != 1 || got[0] != origin {
						t.Errorf("Access-Control-Allow-Origin = %q, want exactly the gateway's %q", got, origin)
					}
				})
			}
		})
	}
}
