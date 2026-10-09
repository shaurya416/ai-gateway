package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ferro-labs/ai-gateway/internal/admin/model"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
)

// lookupFailKeyStore is a store whose single-key read fails while every other
// operation keeps working: a transient database error on one statement, the
// shape a pooled connection reset or a lock timeout takes. Get answers the way
// SQLStore.Get does for such an error — (nil, false), indistinguishable from a
// missing key — and Lookup reports the failure itself.
type lookupFailKeyStore struct {
	*repository.KeyStore
}

func (s *lookupFailKeyStore) Get(context.Context, string) (*model.APIKey, bool) {
	return nil, false
}

func (s *lookupFailKeyStore) Lookup(_ context.Context, id string) (*model.APIKey, error) {
	return nil, fmt.Errorf("lookup key %s: %w", id, errors.New("db connection lost"))
}

// TestLastAdminGuard_LookupFailureFailsClosed pins that a store which cannot
// say whether the target is an admin key refuses the mutation rather than
// treating the target as absent. Read as absent, the guard skipped the count
// and the mutation that followed removed the only admin key.
func TestLastAdminGuard_LookupFailureFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   func(id string) string
		body   string
	}{
		{name: "delete", method: http.MethodDelete, path: func(id string) string { return "/admin/keys/" + id }},
		{name: "revoke", method: http.MethodPost, path: func(id string) string { return "/admin/keys/" + id + "/revoke" }},
		{name: "de-scope", method: http.MethodPut, path: func(id string) string { return "/admin/keys/" + id }, body: `{"scopes":["read_only"]}`},
		{name: "expire in the past", method: http.MethodPut, path: func(id string) string { return "/admin/keys/" + id }, body: `{"expires_at":"2020-01-01T00:00:00Z"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backing := repository.NewKeyStore()
			_, router := newGuardRouter(t, &lookupFailKeyStore{KeyStore: backing})
			only, err := backing.Create(t.Context(), "only-admin", []string{model.ScopeAdmin}, nil)
			if err != nil {
				t.Fatalf("create admin key: %v", err)
			}

			w := httptest.NewRecorder()
			router.ServeHTTP(w, authedRequest(tc.method, tc.path(only.ID), tc.body, masterCaller()))

			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 when the target cannot be read: %s", w.Code, w.Body.String())
			}
			if count, err := backing.CountAdminKeys(t.Context()); err != nil || count != 1 {
				t.Fatalf("admin keys after refused mutation = %d (err %v), want 1", count, err)
			}
		})
	}
}

// TestGetKey_LookupFailureIs500 pins that a store failure on GET
// /admin/keys/{id} is reported as the server error it is, not as a key that
// does not exist.
func TestGetKey_LookupFailureIs500(t *testing.T) {
	backing := repository.NewKeyStore()
	_, router := newGuardRouter(t, &lookupFailKeyStore{KeyStore: backing})
	key, err := backing.Create(t.Context(), "present", []string{model.ScopeReadOnly}, nil)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	w := httptest.NewRecorder()
	router.ServeHTTP(w, authedRequest(http.MethodGet, "/admin/keys/"+key.ID, "", masterCaller()))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for a store that could not answer: %s", w.Code, w.Body.String())
	}
}
