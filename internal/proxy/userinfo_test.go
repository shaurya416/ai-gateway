package proxy

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/providers/core"
	ollamapkg "github.com/ferro-labs/ai-gateway/providers/ollama"
)

// userinfoUpstream records the Authorization header of every request it
// receives and answers 401, quoting that header back in WWW-Authenticate the
// way an authenticating proxy that echoes what it was handed would.
func userinfoUpstream(t *testing.T) (*httptest.Server, <-chan string) {
	t.Helper()
	seen := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		seen <- got
		w.Header().Set("WWW-Authenticate", `Basic realm="proxy", error="rejected `+got+`"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

// withUserinfo returns rawURL with user and password set as its userinfo.
func withUserinfo(t *testing.T, rawURL, user, password string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}

// A base URL's userinfo is how it points at a proxy that authenticates with
// HTTP Basic, and the provider's own client honours it: Ollama sends no
// credential of its own, so OLLAMA_HOST=https://user:pass@host reaches that
// proxy authenticated on chat. The forwards rewrote the request onto the base
// URL with httputil.ProxyRequest.SetURL, which copies the scheme, host and path
// and drops the userinfo, and the transport they hand the request to does not
// read userinfo anyway. Every pass-through forward to the same host therefore
// arrived anonymous and was refused by the proxy chat had just authenticated to.
func TestForwardsCarryBaseURLUserinfo(t *testing.T) {
	const user, password = "gateway", "basic-auth-proxy-password" //nolint:gosec // G101: the userinfo shape under test, not a credential
	wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))

	t.Run("chat-parity", func(t *testing.T) {
		up, seen := userinfoUpstream(t)
		p, err := ollamapkg.New(withUserinfo(t, up.URL, user, password), nil)
		if err != nil {
			t.Fatalf("ollama.New: %v", err)
		}
		_, _ = p.Complete(t.Context(), core.Request{Model: "llama3.2", Messages: []core.Message{{Role: "user", Content: "hi"}}})
		if got := <-seen; got != wantBasic {
			t.Fatalf("chat Authorization = %q, want %q (the reference the forwards must match)", got, wantBasic)
		}
	})

	t.Run("pass-through", func(t *testing.T) {
		up, seen := userinfoUpstream(t)
		p, err := ollamapkg.New(withUserinfo(t, up.URL, user, password), nil)
		if err != nil {
			t.Fatalf("ollama.New: %v", err)
		}
		reg := providers.NewRegistry()
		reg.Register(p)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
		req.Header.Set("X-Provider", "ollama")
		req.Header.Set("Authorization", "Bearer client-gateway-key")
		req.ContentLength = 2
		rec := httptest.NewRecorder()
		Handler(reg)(rec, req)

		if got := <-seen; got != wantBasic {
			t.Errorf("upstream Authorization = %q, want %q from the base URL's userinfo", got, wantBasic)
		}
		if echoed := rec.Header().Get("WWW-Authenticate"); strings.Contains(echoed, strings.TrimPrefix(wantBasic, "Basic ")) {
			t.Errorf("client received the injected Basic credential: %s", echoed)
		}
	})

	t.Run("fixed-target", func(t *testing.T) {
		up, seen := userinfoUpstream(t)
		target, err := url.Parse(withUserinfo(t, up.URL, user, password) + "/openai/v1")
		if err != nil {
			t.Fatalf("parse target: %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/files", nil)
		rec := httptest.NewRecorder()
		_ = forwardFixedTarget(rec, req, target, map[string]string{"api-key": "azure-resource-key-0123456789"}, "azure-openai", false, nil)

		if got := <-seen; got != wantBasic {
			t.Errorf("upstream Authorization = %q, want %q from the base URL's userinfo", got, wantBasic)
		}
		if echoed := rec.Header().Get("WWW-Authenticate"); strings.Contains(echoed, strings.TrimPrefix(wantBasic, "Basic ")) {
			t.Errorf("client received the injected Basic credential: %s", echoed)
		}
	})

	// A provider that sends an Authorization header of its own keeps it, as it
	// does on chat: the userinfo fills the header only when nothing else does.
	t.Run("provider-credential-wins", func(t *testing.T) {
		up, seen := userinfoUpstream(t)
		target, err := url.Parse(withUserinfo(t, up.URL, user, password) + "/v1")
		if err != nil {
			t.Fatalf("parse target: %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/files", nil)
		rec := httptest.NewRecorder()
		_ = forwardFixedTarget(rec, req, target, map[string]string{"Authorization": "Bearer sk-provider-0123456789"}, "openai", false, nil)

		if got := <-seen; got != "Bearer sk-provider-0123456789" {
			t.Errorf("upstream Authorization = %q, want the provider's own bearer credential", got)
		}
	})
}
