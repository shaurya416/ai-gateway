package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ferro-labs/ai-gateway/internal/admin/model"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
)

// mintSession exchanges a credential for a dashboard session token through the
// real sign-in route.
func mintSession(t *testing.T, router http.Handler, credential string) string {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, tokenRequest(http.MethodPost, "/admin/session", "", credential))
	if w.Code != http.StatusCreated {
		t.Fatalf("sign-in status = %d, want 201: %s", w.Code, w.Body.String())
	}
	var body struct {
		Token string `json:"token"`
	}
	decodeJSON(t, w.Body, &body)
	return body.Token
}

// assertStoreUnavailable checks the answer to a request whose credential could
// not be checked: a retryable 5xx naming the cause, never the 401 that tells a
// caller its credential is bad.
func assertStoreUnavailable(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: a store that cannot answer has not rejected the credential: %s", w.Code, w.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, w.Body.String())
	}
	if body.Error.Code != "credential_store_unavailable" {
		t.Errorf("code = %q, want credential_store_unavailable", body.Error.Code)
	}
}

// TestAuth_KeyStoreOutageIsNotARejectedCredential pins that a key store which
// cannot be read is reported as the outage it is. Every path folded the
// failure into "not found": a stored key was answered 401 invalid_api_key, a
// dashboard session 401 invalid_session — which signs the operator out — and a
// sign-in was recorded as a denied credential. No OpenAI SDK retries a 401, so
// a database blip became a hard failure on every request until it ended.
func TestAuth_KeyStoreOutageIsNotARejectedCredential(t *testing.T) {
	const masterKey = "test-master-key"
	keys, err := repository.NewSQLiteStore(t.Context(), filepath.Join(t.TempDir(), "keys.db"))
	if err != nil {
		t.Fatalf("open key store: %v", err)
	}
	t.Cleanup(func() { _ = keys.Close() })
	stored, err := keys.Create(t.Context(), "operator", []string{model.ScopeAdmin}, nil)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	_, router := routerWithStores(keys, repository.NewSessionStore(), masterKey)
	token := mintSession(t, router, stored.Key)

	// The outage: every statement against the key store now fails.
	if err := keys.Close(); err != nil {
		t.Fatalf("close key store: %v", err)
	}

	t.Run("stored API key", func(t *testing.T) {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, tokenRequest(http.MethodGet, "/admin/health", "", stored.Key))
		assertStoreUnavailable(t, w)
	})

	t.Run("session minted from a stored key", func(t *testing.T) {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, tokenRequest(http.MethodGet, "/admin/health", "", token))
		assertStoreUnavailable(t, w)
	})

	t.Run("sign-in with a stored key", func(t *testing.T) {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, tokenRequest(http.MethodPost, "/admin/session", "", stored.Key))
		assertStoreUnavailable(t, w)
	})

	// The master key never reaches the store, so it still authenticates: the
	// outage answer is about the store, not a blanket refusal.
	t.Run("master key", func(t *testing.T) {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, tokenRequest(http.MethodGet, "/admin/health", "", masterKey))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
	})
}

// TestAuth_SessionStoreOutageIsNotAnExpiredSession pins the session store's
// half: a session table that cannot be read has not said the session expired,
// so the dashboard must not be told to sign out.
func TestAuth_SessionStoreOutageIsNotAnExpiredSession(t *testing.T) {
	sessions, err := repository.NewSQLiteSessionStore(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open session store: %v", err)
	}
	t.Cleanup(func() { _ = sessions.Close() })
	keys := repository.NewKeyStore()
	stored, err := keys.Create(t.Context(), "operator", []string{model.ScopeAdmin}, nil)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	_, router := routerWithStores(keys, sessions, "")
	token := mintSession(t, router, stored.Key)

	if err := sessions.Close(); err != nil {
		t.Fatalf("close session store: %v", err)
	}

	w := httptest.NewRecorder()
	router.ServeHTTP(w, tokenRequest(http.MethodGet, "/admin/health", "", token))
	assertStoreUnavailable(t, w)
}
