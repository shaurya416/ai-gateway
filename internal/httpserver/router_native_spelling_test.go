package httpserver_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
	"github.com/ferro-labs/ai-gateway/internal/httpserver"
	"github.com/ferro-labs/ai-gateway/providers"
	openaipkg "github.com/ferro-labs/ai-gateway/providers/openai"

	// The max-token guardrail is what a pass-through spelling walked past.
	_ "github.com/ferro-labs/ai-gateway/plugin/maxtoken"
)

// decodingUpstream is an OpenAI-compatible upstream that routes the way HTTP
// servers do: on the decoded path, with a trailing slash dropped. It answers
// every POST with the OpenAI conformance payload and records what reached it.
type decodingUpstream struct {
	mu       sync.Mutex
	requests []string
}

func (u *decodingUpstream) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.requests...)
}

func newDecodingUpstream(t *testing.T) (*decodingUpstream, *httptest.Server) {
	t.Helper()
	payload, err := os.ReadFile("../../providers/openai/testdata/chat.response.json")
	if err != nil {
		t.Fatalf("read openai chat fixture: %v", err)
	}
	up := &decodingUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.mu.Lock()
		up.requests = append(up.requests, r.Method+" "+r.RequestURI)
		up.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return up, srv
}

// A route the gateway serves itself is never a pass-through candidate, however
// its path is spelled. The router matched the native routes on the path as
// written, so a trailing slash, a doubled slash or a percent-encoded letter
// missed them and fell to the /v1/* pass-through — which forwards the spelling
// to an upstream that decodes it and serves the very route the gateway owns.
// That request skipped everything the native pipeline applies: the max-token
// guardrail approved a ceiling it never saw, a legacy completions stream the
// gateway refuses reached the provider's own completions route, and Files
// answered from a provider nobody named in batch_target.
func TestRouter_NativeRouteSpellingsAreNotPassthroughCandidates(t *testing.T) {
	t.Setenv("ALLOW_UNAUTHENTICATED_PROXY", "true")
	upstream, upstreamSrv := newDecodingUpstream(t)

	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: openaipkg.Name, Models: []string{"gpt-4o-mini"}}},
		Plugins: []config.PluginConfig{{
			Name:    "max-token",
			Type:    "guardrail",
			Stage:   "before_request",
			Enabled: true,
			Config:  map[string]any{"max_tokens": 16},
		}},
	})
	if err != nil {
		t.Fatalf("New gateway: %v", err)
	}
	if err := gw.LoadPlugins(); err != nil {
		t.Fatalf("load plugins: %v", err)
	}
	p, err := openaipkg.New("sk-test-key", upstreamSrv.URL)
	if err != nil {
		t.Fatalf("build openai provider: %v", err)
	}
	reg := providers.NewRegistry()
	reg.Register(p)
	gw.RegisterProvider(p)
	gateway := httptest.NewServer(httpserver.NewRouter(reg, repository.NewKeyStore(), nil, nil, gw, nil, nil, nil, nil, nil, "", nil))
	defer gateway.Close()

	send := func(t *testing.T, method, path, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, gateway.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("build %s %s: %v", method, path, err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Provider", openaipkg.Name)
		resp, err := gateway.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	const overCap = `{"model":"gpt-4o-mini","max_tokens":500000,"messages":[{"role":"user","content":"hi"}]}`
	const legacyStream = `{"model":"gpt-4o-mini","prompt":"hi","stream":true}`

	refused := []struct {
		name, method, path, body string
		want                     int
	}{
		{"chat with a trailing slash", http.MethodPost, "/v1/chat/completions/", overCap, http.StatusBadRequest},
		{"chat with a doubled slash", http.MethodPost, "/v1//chat/completions", overCap, http.StatusBadRequest},
		{"chat with an encoded letter", http.MethodPost, "/v1/chat/completion%73", overCap, http.StatusBadRequest},
		{"legacy completions with an encoded letter", http.MethodPost, "/v1/%63ompletions", legacyStream, http.StatusBadRequest},
		{"legacy completions with a trailing slash", http.MethodPost, "/v1/completions/", legacyStream, http.StatusBadRequest},
		{"files with an encoded letter", http.MethodGet, "/v1/file%73", "", http.StatusNotImplemented},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			before := len(upstream.seen())
			resp := send(t, tc.method, tc.path, tc.body)
			if resp.StatusCode != tc.want {
				t.Errorf("%s %s = %d, want %d as on the canonical route", tc.method, tc.path, resp.StatusCode, tc.want)
			}
			if got := upstream.seen()[before:]; len(got) != 0 {
				t.Errorf("%s %s reached the upstream as %v; the gateway owns this route and refuses the request", tc.method, tc.path, got)
			}
		})
	}

	// Within the cap, the spelling is served by the native chat pipeline: the
	// response carries the routed surfaces' attribution, and the upstream is
	// called on the provider's own path rather than the caller's spelling.
	t.Run("chat within the cap is served natively", func(t *testing.T) {
		before := len(upstream.seen())
		resp := send(t, http.MethodPost, "/v1/chat/completion%73",
			`{"model":"gpt-4o-mini","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got := resp.Header.Get("X-Gateway-Target"); got != openaipkg.Name {
			t.Errorf("X-Gateway-Target = %q, want %q: the request did not take the routed chat surface", got, openaipkg.Name)
		}
		if got := upstream.seen()[before:]; len(got) != 1 || got[0] != "POST /v1/chat/completions" {
			t.Errorf("upstream saw %v, want exactly [POST /v1/chat/completions]", got)
		}
	})
}
