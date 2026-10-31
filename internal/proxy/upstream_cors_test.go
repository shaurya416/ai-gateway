package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/middleware"
	"github.com/ferro-labs/ai-gateway/providers"
)

// unconditionalCORSUpstream answers every request with a CORS grant of its
// own, whether or not the request carries an Origin: the default of the cors
// package for Express (`app.use(cors())`), and of an nginx `add_header
// Access-Control-Allow-Origin *` in front of a server.
func unconditionalCORSUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Expose-Headers", "X-Upstream-Internal")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A browser application on a forward is answered under the gateway's CORS
// policy alone. An upstream that grants CORS on every response had its grant
// relayed beside the gateway's: an origin on CORS_ORIGINS got two
// Access-Control-Allow-Origin values, which a browser refuses, and an origin
// the gateway denies by withholding the header got the upstream's "*" instead.
func TestForwardsRelayNoUpstreamCORSGrant(t *testing.T) {
	const allowed = "https://app.example.com"

	up := unconditionalCORSUpstream(t)
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
		srv := httptest.NewServer(middleware.CORS(allowed)(s.h))
		t.Cleanup(srv.Close)

		for _, origin := range []string{allowed, "https://elsewhere.example.com"} {
			t.Run(name+" "+origin, func(t *testing.T) {
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
					t.Fatalf("status = %d, want 200", resp.StatusCode)
				}
				want := []string(nil)
				if origin == allowed {
					want = []string{allowed}
				}
				if got := resp.Header.Values("Access-Control-Allow-Origin"); strings.Join(got, "|") != strings.Join(want, "|") {
					t.Errorf("Access-Control-Allow-Origin = %q, want %q: only the gateway's CORS policy answers a browser", got, want)
				}
				if got := resp.Header.Values("Access-Control-Allow-Credentials"); len(got) != 0 {
					t.Errorf("Access-Control-Allow-Credentials = %q, want none: the gateway grants no credentialed CORS", got)
				}
				for _, v := range resp.Header.Values("Access-Control-Expose-Headers") {
					if strings.Contains(v, "X-Upstream-Internal") {
						t.Errorf("Access-Control-Expose-Headers = %q: the upstream's exposure list was relayed", resp.Header.Values("Access-Control-Expose-Headers"))
					}
				}
			})
		}
	}
}
