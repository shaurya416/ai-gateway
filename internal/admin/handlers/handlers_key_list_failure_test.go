package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ferro-labs/ai-gateway/internal/admin/model"
)

// TestKeyListing_StoreFailureIs500 pins that a key store which cannot be read
// is reported as the failure it is on every route that lists keys, never as a
// store holding no keys.
//
// The master key is what reaches these routes during the outage: it is checked
// before the store, so it authenticates while every stored key cannot — which
// makes it the credential an operator reaches for at exactly this moment. An
// empty 200 then tells them the keys are gone.
func TestKeyListing_StoreFailureIs500(t *testing.T) {
	for _, path := range []string{"/admin/keys", "/admin/keys/usage", "/admin/dashboard"} {
		t.Run(path, func(t *testing.T) {
			store := newSQLiteTestStore(t)
			_, router := newGuardRouter(t, store)
			if _, err := store.Create(t.Context(), "present", []string{model.ScopeAdmin}, nil); err != nil {
				t.Fatalf("create key: %v", err)
			}
			// The database going away, as an outage or a lost connection pool
			// does: every statement on it now fails.
			if err := store.Close(); err != nil {
				t.Fatalf("close store: %v", err)
			}

			w := httptest.NewRecorder()
			router.ServeHTTP(w, authedRequest(http.MethodGet, path, "", masterCaller()))

			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 for a key store that could not be read: %s", w.Code, w.Body.String())
			}
		})
	}
}
