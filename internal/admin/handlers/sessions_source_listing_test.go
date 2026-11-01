package handlers

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/internal/admin/model"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
	"github.com/go-chi/chi/v5"
)

// listedSessionCredentials returns the credential ids GET /admin/sessions
// reports as signed in, asked with caller's API key.
func listedSessionCredentials(t *testing.T, r chi.Router, caller *model.APIKey) []string {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet, "/admin/sessions", "", caller))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/sessions = %d, want 200: %s", w.Code, w.Body.String())
	}
	var body struct {
		Data []model.Session `json:"data"`
	}
	decodeJSON(t, w.Body, &body)
	ids := make([]string, 0, len(body.Data))
	for _, s := range body.Data {
		ids = append(ids, s.CredentialID)
	}
	return ids
}

// TestListSessions_OmitsSessionsWhoseCredentialCannotAuthenticate pins that the
// sessions page lists the operators who are signed in, not every row inside its
// time bounds. A session is refused on every request once the key it was minted
// from is revoked, deleted or expired, so listing it told the admin who had just
// revoked a departing operator's key that the operator was still signed in.
func TestListSessions_OmitsSessionsWhoseCredentialCannotAuthenticate(t *testing.T) {
	cases := map[string]func(t *testing.T, keys repository.Store, id string){
		"revoked": func(t *testing.T, keys repository.Store, id string) {
			if err := keys.Revoke(t.Context(), id); err != nil {
				t.Fatalf("revoke: %v", err)
			}
		},
		"deleted": func(t *testing.T, keys repository.Store, id string) {
			if err := keys.Delete(t.Context(), id); err != nil {
				t.Fatalf("delete: %v", err)
			}
		},
		"expired": func(t *testing.T, keys repository.Store, id string) {
			past := time.Now().Add(-time.Minute)
			if err := keys.SetExpiration(t.Context(), id, &past); err != nil {
				t.Fatalf("expire: %v", err)
			}
		},
	}
	for name, retire := range cases {
		for storeName, sessions := range newTestSessionStores(t) {
			t.Run(name+"/"+storeName, func(t *testing.T) {
				keys := repository.NewKeyStore()
				h, r := routerWithStores(keys, sessions, "")
				observer := createAdminKey(t, h)
				departing := createTestKey(t, h, "departing", []string{model.ScopeAdmin}, nil)
				staying := createTestKey(t, h, "staying", []string{model.ScopeReadOnly}, nil)

				departingToken := exchangeSession(t, r, departing.Key)
				exchangeSession(t, r, staying.Key)

				retire(t, keys, departing.ID)
				assertSessionStatus(t, r, departingToken, http.StatusUnauthorized)

				got := listedSessionCredentials(t, r, observer)
				if len(got) != 1 || got[0] != staying.ID {
					t.Fatalf("GET /admin/sessions listed credentials %v, want only %q: a session its key can no longer back is not signed in", got, staying.ID)
				}
			})
		}
	}
}

// TestListSessions_OmitsSessionsMintedFromARotatedMasterKey is the master-key
// half: rotating MASTER_KEY ends the sessions minted from the old value, so a
// persistent session store read after the rotation must not list them.
func TestListSessions_OmitsSessionsMintedFromARotatedMasterKey(t *testing.T) {
	for storeName, sessions := range newTestSessionStores(t) {
		t.Run(storeName, func(t *testing.T) {
			keys := repository.NewKeyStore()
			_, before := routerWithStores(keys, sessions, "master-before-rotation")
			oldToken := exchangeSession(t, before, "master-before-rotation")

			// The same stores served after a restart with a rotated key.
			h, after := routerWithStores(keys, sessions, "master-after-rotation")
			observer := createAdminKey(t, h)
			assertSessionStatus(t, after, oldToken, http.StatusUnauthorized)

			if got := listedSessionCredentials(t, after, observer); len(got) != 0 {
				t.Fatalf("GET /admin/sessions listed %v after MASTER_KEY rotation, want none", got)
			}
		})
	}
}

// TestRevokeAllSessions_CountsOnlySessionsThatCouldAuthenticate pins that
// signing everyone out reports the operators it signed out, the same set
// GET /admin/sessions lists. A session whose key had been revoked was already
// refused on every request, and counting it reported one operator more than
// were signed in — in the response the dashboard shows and in the audit row.
func TestRevokeAllSessions_CountsOnlySessionsThatCouldAuthenticate(t *testing.T) {
	for storeName, sessions := range newTestSessionStores(t) {
		t.Run(storeName, func(t *testing.T) {
			keys := repository.NewKeyStore()
			h, r := routerWithStores(keys, sessions, "")
			audit := repository.NewMemoryAuditStore()
			h.Audit = audit
			operator := createAdminKey(t, h)
			departing := createTestKey(t, h, "departing", []string{model.ScopeAdmin}, nil)

			exchangeSession(t, r, operator.Key)
			exchangeSession(t, r, departing.Key)
			if err := keys.Revoke(t.Context(), departing.ID); err != nil {
				t.Fatalf("revoke: %v", err)
			}

			w := httptest.NewRecorder()
			r.ServeHTTP(w, authedRequest(http.MethodDelete, "/admin/sessions", "", operator))
			if w.Code != http.StatusOK {
				t.Fatalf("DELETE /admin/sessions = %d, want 200: %s", w.Code, w.Body.String())
			}
			var body struct {
				Revoked int `json:"revoked"`
			}
			decodeJSON(t, w.Body, &body)
			if body.Revoked != 1 {
				t.Fatalf("revoked = %d, want 1: only the operator's session could still authenticate", body.Revoked)
			}

			entries := auditEntries(t, audit, "session.revoke_all")
			if len(entries) != 1 || entries[0].Detail != `{"count":1}` {
				t.Fatalf("session.revoke_all audit rows = %+v, want one with count 1", entries)
			}
		})
	}
}

// TestSessionRoutes_KeyStoreOutage pins what the two routes do when a session's
// source credential cannot be checked. The listing does not guess, in either
// direction; signing everyone out is the emergency control, so it goes ahead and
// counts the session it could not check.
func TestSessionRoutes_KeyStoreOutage(t *testing.T) {
	const masterKey = "outage-master-key"
	keys, err := repository.NewSQLiteStore(t.Context(), filepath.Join(t.TempDir(), "keys.db"))
	if err != nil {
		t.Fatalf("open key store: %v", err)
	}
	stored, err := keys.Create(t.Context(), "operator", []string{model.ScopeAdmin}, nil)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	_, r := routerWithStores(keys, repository.NewSessionStore(), masterKey)
	exchangeSession(t, r, stored.Key)
	if err := keys.Close(); err != nil {
		t.Fatalf("close key store: %v", err)
	}
	master := &model.APIKey{Key: masterKey}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet, "/admin/sessions", "", master))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("GET /admin/sessions with the key store down = %d, want 500: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodDelete, "/admin/sessions", "", master))
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE /admin/sessions with the key store down = %d, want 200: %s", w.Code, w.Body.String())
	}
	var body struct {
		Revoked int `json:"revoked"`
	}
	decodeJSON(t, w.Body, &body)
	if body.Revoked != 1 {
		t.Fatalf("revoked = %d, want 1: a session whose key could not be checked is counted", body.Revoked)
	}
}
