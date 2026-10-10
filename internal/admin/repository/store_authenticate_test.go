package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/internal/admin/model"
)

// TestStoreAuthenticate_RejectionIsErrInvalidCredential pins that every reason
// a key does not authenticate is reported as model.ErrInvalidCredential on
// both backends. The auth middleware answers anything else as a store outage,
// so a rejection that escaped this sentinel would turn a revoked key's 401 into
// a 503 the caller retries.
func TestStoreAuthenticate_RejectionIsErrInvalidCredential(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store Store
	}{
		{name: "memory", store: NewKeyStore()},
		{name: "sqlite", store: newSQLiteTestStore(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			live, err := tc.store.Create(ctx, "live", []string{model.ScopeAdmin}, nil)
			if err != nil {
				t.Fatalf("create live key: %v", err)
			}
			revoked, err := tc.store.Create(ctx, "revoked", []string{model.ScopeAdmin}, nil)
			if err != nil {
				t.Fatalf("create revoked key: %v", err)
			}
			if err := tc.store.Revoke(ctx, revoked.ID); err != nil {
				t.Fatalf("revoke: %v", err)
			}
			past := time.Now().Add(-time.Hour)
			expired, err := tc.store.Create(ctx, "expired", []string{model.ScopeAdmin}, &past)
			if err != nil {
				t.Fatalf("create expired key: %v", err)
			}

			got, err := tc.store.Authenticate(ctx, live.Key)
			if err != nil || got == nil || got.ID != live.ID {
				t.Fatalf("Authenticate(live) = %v, %v; want the key", got, err)
			}
			for name, presented := range map[string]string{
				"unknown": "fgw_not-a-key",
				"empty":   "",
				"revoked": revoked.Key,
				"expired": expired.Key,
			} {
				if _, err := tc.store.Authenticate(ctx, presented); !errors.Is(err, model.ErrInvalidCredential) {
					t.Errorf("Authenticate(%s) error = %v, want ErrInvalidCredential", name, err)
				}
			}
		})
	}
}

// TestSQLStore_Authenticate_ReadFailureIsNotARejection pins the other half: a
// key read that fails is a failure, never a rejected credential.
func TestSQLStore_Authenticate_ReadFailureIsNotARejection(t *testing.T) {
	store := newSQLiteTestStore(t)
	created, err := store.Create(t.Context(), "admin", []string{model.ScopeAdmin}, nil)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if err := store.stmtGetByHash.Close(); err != nil {
		t.Fatalf("close stmtGetByHash: %v", err)
	}

	_, err = store.Authenticate(t.Context(), created.Key)
	if err == nil || errors.Is(err, model.ErrInvalidCredential) {
		t.Fatalf("Authenticate on a failed read = %v, want a non-ErrInvalidCredential error", err)
	}
}

// TestSessionStoreAuthenticate_RejectionIsErrInvalidCredential is the session
// store's counterpart: unknown, empty, expired and deleted tokens are
// rejections on every backend, and a session table that cannot be read is not.
func TestSessionStoreAuthenticate_RejectionIsErrInvalidCredential(t *testing.T) {
	for name, store := range newTestSessionStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			_, live, err := store.CreateSession(ctx, "live", "cred", []string{model.ScopeAdmin}, DefaultSessionTTL)
			if err != nil {
				t.Fatalf("CreateSession(live): %v", err)
			}
			_, expired, err := store.CreateSession(ctx, "expired", "cred", []string{model.ScopeAdmin}, -time.Second)
			if err != nil {
				t.Fatalf("CreateSession(expired): %v", err)
			}
			deletedSess, deleted, err := store.CreateSession(ctx, "deleted", "cred", []string{model.ScopeAdmin}, DefaultSessionTTL)
			if err != nil {
				t.Fatalf("CreateSession(deleted): %v", err)
			}
			if err := store.DeleteSession(ctx, deletedSess.ID); err != nil {
				t.Fatalf("DeleteSession: %v", err)
			}

			if got, err := store.AuthenticateSession(ctx, live); err != nil || got.Subject != "live" {
				t.Fatalf("AuthenticateSession(live) = %v, %v; want the session", got, err)
			}
			for name, token := range map[string]string{
				"unknown": model.SessionTokenPrefix + "not-a-token",
				"empty":   "",
				"expired": expired,
				"deleted": deleted,
			} {
				if _, err := store.AuthenticateSession(ctx, token); !errors.Is(err, model.ErrInvalidCredential) {
					t.Errorf("AuthenticateSession(%s) error = %v, want ErrInvalidCredential", name, err)
				}
			}
		})
	}

	for name, c := range newTestSQLSessionStores(t) {
		t.Run(name+"/read failure", func(t *testing.T) {
			_, token, err := c.store.CreateSession(context.Background(), "s", "cred", []string{model.ScopeAdmin}, DefaultSessionTTL)
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			if err := c.db.Close(); err != nil {
				t.Fatalf("close db: %v", err)
			}
			if _, err := c.store.AuthenticateSession(context.Background(), token); err == nil || errors.Is(err, model.ErrInvalidCredential) {
				t.Fatalf("AuthenticateSession on a closed store = %v, want a non-ErrInvalidCredential error", err)
			}
		})
	}
}
