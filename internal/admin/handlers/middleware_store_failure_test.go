package handlers

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/internal/admin/model"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
	"github.com/ferro-labs/ai-gateway/internal/sqldb"
)

// serveSessionBearer runs one request carrying token through the session-aware
// auth middleware and reports the status and whether the protected handler ran.
func serveSessionBearer(t *testing.T, keys repository.Store, sessions repository.SessionStore, token string) (int, bool) {
	t.Helper()
	reached := false
	protected := AuthMiddlewareWithSessions(keys, sessions, "")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/health", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, req)
	return w.Code, reached
}

// TestSessionAuth_SessionStoreFailureIsNotASignOut pins that a session store
// which cannot answer is reported as the server's failure, not as a dead
// session. A 401 is what signs the dashboard out — it discards the token it
// holds — so a database blip on the session read signed every operator out of
// sessions that were still valid.
func TestSessionAuth_SessionStoreFailureIsNotASignOut(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	sessions, err := repository.NewSQLSessionStore(t.Context(), db, sqldb.SQLite)
	if err != nil {
		t.Fatalf("NewSQLSessionStore: %v", err)
	}
	keys := repository.NewKeyStore()
	key, err := keys.Create(t.Context(), "operator", []string{model.ScopeAdmin}, nil)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	_, token, err := sessions.CreateSession(t.Context(), "operator", key.ID, key.Scopes, time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// While the store answers, an unknown token is still a dead session.
	if code, _ := serveSessionBearer(t, keys, sessions, model.SessionTokenPrefix+"unknown"); code != http.StatusUnauthorized {
		t.Fatalf("unknown token: status = %d, want 401", code)
	}

	// The session row is intact; only the store's ability to read it is gone.
	_ = db.Close()
	code, reached := serveSessionBearer(t, keys, sessions, token)
	if reached {
		t.Fatal("protected handler ran although the session could not be checked")
	}
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the session store cannot answer — a 401 signs the dashboard out of a live session", code)
	}
}

// TestSessionAuth_CredentialStoreFailureIsNotASignOut is the same rule for the
// second read the session path makes: the key store re-check of the credential
// the session was minted from. Read through Get, a store failure was
// indistinguishable from a deleted key and answered 401.
func TestSessionAuth_CredentialStoreFailureIsNotASignOut(t *testing.T) {
	backing := repository.NewKeyStore()
	key, err := backing.Create(t.Context(), "operator", []string{model.ScopeAdmin}, nil)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	sessions := repository.NewSessionStore()
	_, token, err := sessions.CreateSession(t.Context(), "operator", key.ID, key.Scopes, time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	code, reached := serveSessionBearer(t, &lookupFailKeyStore{KeyStore: backing}, sessions, token)
	if reached {
		t.Fatal("protected handler ran although the credential could not be checked")
	}
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the key store cannot answer the credential re-check", code)
	}

	// A credential that is genuinely gone still ends the session.
	if err := backing.Delete(t.Context(), key.ID); err != nil {
		t.Fatalf("delete key: %v", err)
	}
	if code, _ := serveSessionBearer(t, backing, sessions, token); code != http.StatusUnauthorized {
		t.Fatalf("deleted credential: status = %d, want 401", code)
	}
}
