package httpserver_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
	"github.com/ferro-labs/ai-gateway/internal/httpserver"
	"github.com/ferro-labs/ai-gateway/providers"
)

// stubProvider is a minimal provider stub that satisfies providers.Provider.
type stubProvider struct{}

func (stubProvider) Name() string               { return "stub" }
func (stubProvider) ConfiguredModels() []string { return []string{"stub-model"} }
func (stubProvider) SupportsModel(m string) bool {
	return m == "stub-model"
}
func (stubProvider) Models() []providers.ModelInfo {
	return []providers.ModelInfo{{ID: "stub-model", Object: "model", OwnedBy: "stub"}}
}
func (stubProvider) Complete(_ context.Context, _ providers.Request) (*providers.Response, error) {
	return &providers.Response{
		ID:    "stub-1",
		Model: "stub-model",
		Choices: []providers.Choice{{
			Message:      providers.Message{Role: "assistant", Content: "ok"},
			FinishReason: "stop",
		}},
	}, nil
}

// buildTestRouter creates a router wired with the given gateway and a stub registry.
// It always enables unauthenticated proxy access (test only).
func buildTestRouter(t *testing.T, gw *aigateway.Gateway) http.Handler {
	t.Helper()
	t.Setenv("ALLOW_UNAUTHENTICATED_PROXY", "true")

	reg := providers.NewRegistry()
	reg.Register(stubProvider{})

	ks := repository.NewKeyStore()
	return httpserver.NewRouter(reg, ks, nil, nil, gw, nil, nil, nil, nil, nil, "", nil)
}

// TestBodySizeLimit_TooLarge_Returns413 verifies that a POST body exceeding the configured
// MaxRequestBytes limit results in HTTP 413 Request Entity Too Large.
func TestBodySizeLimit_TooLarge_Returns413(t *testing.T) {
	const smallLimit = 64 // bytes — well below a real chat request

	gw, err := newTestGateway(t, config.Config{
		MaxRequestBytes: smallLimit,
		Strategy:        config.StrategyConfig{Mode: config.ModeSingle},
		Targets:         []config.Target{{VirtualKey: "stub"}},
	})

	if err != nil {
		t.Fatalf("New gateway: %v", err)
	}

	r := buildTestRouter(t, gw)

	// Build a body that is definitely larger than the 64-byte limit.
	body := `{"model":"stub-model","messages":[{"role":"user","content":"` + strings.Repeat("x", 200) + `"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected HTTP 413, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// TestBodySizeLimit_UnderLimit_NotRejected verifies that a small valid body is not
// rejected by the size limit middleware (it may fail for other reasons such as
// no matching provider, but must not be a 413).
func TestBodySizeLimit_UnderLimit_NotRejected(t *testing.T) {
	const largeLimit = 10 * 1024 * 1024 // 10 MiB default

	gw, err := newTestGateway(t, config.Config{
		MaxRequestBytes: largeLimit,
		Strategy:        config.StrategyConfig{Mode: config.ModeSingle},
		Targets:         []config.Target{{VirtualKey: "stub"}},
	})

	if err != nil {
		t.Fatalf("New gateway: %v", err)
	}

	r := buildTestRouter(t, gw)

	// A minimal valid-looking chat body (under the 10 MiB limit).
	body := `{"model":"stub-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code == http.StatusRequestEntityTooLarge {
		t.Errorf("small body should not produce 413, got %d", w.Code)
	}
}

// TestBodySizeLimit_FollowsRuntimeConfigChange verifies that a max_request_bytes
// change applied to a running gateway is enforced without a restart, in both
// directions.
//
// Lowering is the direction that matters for safety: the cap used to be read
// once while the router was built and captured in the middleware closure, so a
// lowered cap was accepted, reported by GET /admin/config, and ignored — a body
// far above the new limit was still admitted until the process restarted.
func TestBodySizeLimit_FollowsRuntimeConfigChange(t *testing.T) {
	const (
		smallLimit = 64
		largeLimit = 1 << 20
	)

	cfg := config.Config{
		MaxRequestBytes: largeLimit,
		Strategy:        config.StrategyConfig{Mode: config.ModeSingle},
		Targets:         []config.Target{{VirtualKey: "stub"}},
	}
	gw, err := newTestGateway(t, cfg)
	if err != nil {
		t.Fatalf("New gateway: %v", err)
	}
	r := buildTestRouter(t, gw)

	// A body comfortably over smallLimit and comfortably under largeLimit.
	body := `{"model":"stub-model","messages":[{"role":"user","content":"` + strings.Repeat("x", 200) + `"}]}`

	post := func() int {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	if code := post(); code == http.StatusRequestEntityTooLarge {
		t.Fatalf("body under the boot limit should not 413, got %d", code)
	}

	// Lower the cap on the running gateway.
	lowered := cfg
	lowered.MaxRequestBytes = smallLimit
	if err := gw.ReloadConfig(t.Context(), lowered); err != nil {
		t.Fatalf("ReloadConfig(lowered): %v", err)
	}
	if code := post(); code != http.StatusRequestEntityTooLarge {
		t.Errorf("after lowering max_request_bytes to %d, expected 413, got %d", smallLimit, code)
	}

	// Raise it back.
	if err := gw.ReloadConfig(t.Context(), cfg); err != nil {
		t.Fatalf("ReloadConfig(raised): %v", err)
	}
	if code := post(); code == http.StatusRequestEntityTooLarge {
		t.Errorf("after raising max_request_bytes back to %d, expected no 413, got %d", largeLimit, code)
	}
}

// TestBodySizeLimit_OmittedFallsBackToDefault verifies that clearing
// max_request_bytes at runtime restores the built-in default rather than
// leaving an unbounded body reader behind.
func TestBodySizeLimit_OmittedFallsBackToDefault(t *testing.T) {
	cfg := config.Config{
		MaxRequestBytes: 64,
		Strategy:        config.StrategyConfig{Mode: config.ModeSingle},
		Targets:         []config.Target{{VirtualKey: "stub"}},
	}
	gw, err := newTestGateway(t, cfg)
	if err != nil {
		t.Fatalf("New gateway: %v", err)
	}
	r := buildTestRouter(t, gw)

	cleared := cfg
	cleared.MaxRequestBytes = 0
	if err := gw.ReloadConfig(t.Context(), cleared); err != nil {
		t.Fatalf("ReloadConfig(cleared): %v", err)
	}

	// Under the 10 MiB default, over the 64-byte boot value.
	body := `{"model":"stub-model","messages":[{"role":"user","content":"` + strings.Repeat("x", 200) + `"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusRequestEntityTooLarge {
		t.Errorf("omitted max_request_bytes should apply the %d-byte default, got 413", config.DefaultMaxRequestBytes)
	}
}

// gatewayConfigs serves the admin config routes from the gateway itself; the
// gateway has no backing store to ping.
type gatewayConfigs struct{ *aigateway.Gateway }

func (gatewayConfigs) Ping(context.Context) error { return nil }

// TestBodySizeLimit_AdminWritesAnswer413 verifies that the admin write
// endpoints answer a body over max_request_bytes with the 413 the limit is
// documented to produce. Each decoded the body itself and reported the cap
// tripping as a malformed body — 400 "invalid request body" — which tells the
// operator to fix JSON that was never wrong.
func TestBodySizeLimit_AdminWritesAnswer413(t *testing.T) {
	const masterKey = "test-master-key"
	gw, err := newTestGateway(t, config.Config{
		MaxRequestBytes: 256,
		Strategy:        config.StrategyConfig{Mode: config.ModeSingle},
		Targets:         []config.Target{{VirtualKey: "stub"}},
	})
	if err != nil {
		t.Fatalf("New gateway: %v", err)
	}
	reg := providers.NewRegistry()
	reg.Register(stubProvider{})
	r := httpserver.NewRouter(reg, repository.NewKeyStore(), nil, nil, gw, gatewayConfigs{gw}, nil, nil, nil, nil, masterKey, nil)

	send := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+masterKey)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// Under the cap the same route answers normally, so the 413 below is the
	// limit's answer and not the route's.
	if w := send(http.MethodPost, "/admin/keys", `{"name":"small","scopes":["read_only"]}`); w.Code != http.StatusCreated {
		t.Fatalf("POST /admin/keys under the limit: status = %d, want 201: %s", w.Code, w.Body.String())
	}

	padding := strings.Repeat("x", 512)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/admin/keys", `{"name":"` + padding + `"}`},
		{http.MethodPut, "/admin/keys/some-key", `{"name":"` + padding + `"}`},
		{http.MethodPost, "/admin/config", `{"strategy":{"mode":"single"},"targets":[{"virtual_key":"` + padding + `"}]}`},
		{http.MethodPut, "/admin/config", `{"strategy":{"mode":"single"},"targets":[{"virtual_key":"` + padding + `"}]}`},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := send(tc.method, tc.path, tc.body)
			if w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"code":"request_too_large"`) {
				t.Errorf("body = %s, want code request_too_large", w.Body.String())
			}
		})
	}
}
