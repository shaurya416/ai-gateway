package repository

import (
	"errors"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/internal/admin/model"
)

// TestSQLStore_RotateKey_FailedReadLeavesTheOldSecretWorking pins that a
// rotation reported as failed did not happen.
//
// The new secret exists only in the response to the call that minted it, so a
// rotation that commits and then answers with an error destroys the old secret
// and never hands over the new one: every client holding the key is locked out
// while the operator, told the rotation failed, has no reason to think anything
// changed.
func TestSQLStore_RotateKey_FailedReadLeavesTheOldSecretWorking(t *testing.T) {
	store := newSQLiteTestStore(t)
	created, err := store.Create(t.Context(), "service", []string{model.ScopeAdmin}, nil)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// Close the prepared by-id statement to simulate a transient read failure.
	// The rotate and by-hash statements stay open.
	if err := store.stmtGetByID.Close(); err != nil {
		t.Fatalf("close stmtGetByID: %v", err)
	}

	rotated, err := store.RotateKey(t.Context(), created.ID)
	if err == nil {
		t.Fatalf("RotateKey with a failing key read = %+v, nil; want an error", rotated)
	}
	if errors.Is(err, model.ErrKeyNotFound) {
		t.Fatalf("RotateKey error = %v; a failed read is not a missing key", err)
	}

	if _, err := store.Authenticate(t.Context(), created.Key); err != nil {
		t.Fatalf("old secret after a rotation reported as failed: Authenticate = %v; want it still to authenticate", err)
	}
}

// TestSQLStore_RotateKey_ResponseDescribesTheRotatedKey pins what a successful
// rotation hands back: the new secret, which authenticates, together with the
// key's stored attributes and the rotation time.
func TestSQLStore_RotateKey_ResponseDescribesTheRotatedKey(t *testing.T) {
	store := newSQLiteTestStore(t)
	created, err := store.Create(t.Context(), "service", []string{model.ScopeReadOnly}, nil)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	rotated, err := store.RotateKey(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("rotate key: %v", err)
	}
	if rotated.ID != created.ID || rotated.Name != "service" || len(rotated.Scopes) != 1 || rotated.Scopes[0] != model.ScopeReadOnly {
		t.Fatalf("rotated key = %+v; want id %q, name %q, scopes [%s]", rotated, created.ID, "service", model.ScopeReadOnly)
	}
	if !rotated.Active || rotated.RotatedAt == nil {
		t.Fatalf("rotated key active=%v rotated_at=%v; want active and a rotation time", rotated.Active, rotated.RotatedAt)
	}
	if rotated.Key == created.Key {
		t.Fatal("rotated key carries the old secret")
	}

	got, err := store.Authenticate(t.Context(), rotated.Key)
	if err != nil || got.ID != created.ID {
		t.Fatalf("Authenticate(new secret) = %v, %v; want key %q", got, err, created.ID)
	}
	if _, err := store.Authenticate(t.Context(), created.Key); !errors.Is(err, model.ErrInvalidCredential) {
		t.Fatalf("Authenticate(old secret) error = %v; want ErrInvalidCredential", err)
	}

	stored, err := store.Lookup(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if stored.RotatedAt == nil || !stored.RotatedAt.Equal(*rotated.RotatedAt) {
		t.Fatalf("stored rotated_at = %v; want the %v the response reported", stored.RotatedAt, rotated.RotatedAt)
	}
}

// TestStoreRotateKey_RefusesKeyThatCannotAuthenticate pins, on both backends,
// that a revoked or expired key is not given a new secret: the refusal names
// the reason and leaves the stored key as it was.
func TestStoreRotateKey_RefusesKeyThatCannotAuthenticate(t *testing.T) {
	for _, backend := range []struct {
		name  string
		store func(t *testing.T) Store
	}{
		{name: "memory", store: func(*testing.T) Store { return NewKeyStore() }},
		{name: "sqlite", store: func(t *testing.T) Store { return newSQLiteTestStore(t) }},
	} {
		for _, tc := range []struct {
			name    string
			arrange func(t *testing.T, store Store, id string)
			want    error
		}{
			{
				name: "revoked",
				arrange: func(t *testing.T, store Store, id string) {
					if err := store.Revoke(t.Context(), id); err != nil {
						t.Fatalf("revoke: %v", err)
					}
				},
				want: model.ErrKeyRevoked,
			},
			{
				name: "expired",
				arrange: func(t *testing.T, store Store, id string) {
					past := time.Now().Add(-time.Minute)
					if err := store.SetExpiration(t.Context(), id, &past); err != nil {
						t.Fatalf("expire: %v", err)
					}
				},
				want: model.ErrKeyExpired,
			},
		} {
			t.Run(backend.name+"/"+tc.name, func(t *testing.T) {
				store := backend.store(t)
				created, err := store.Create(t.Context(), "service", []string{model.ScopeReadOnly}, nil)
				if err != nil {
					t.Fatalf("create key: %v", err)
				}
				tc.arrange(t, store, created.ID)

				rotated, err := store.RotateKey(t.Context(), created.ID)
				if !errors.Is(err, tc.want) {
					t.Fatalf("RotateKey = %+v, %v; want an error wrapping %v", rotated, err, tc.want)
				}
				stored, err := store.Lookup(t.Context(), created.ID)
				if err != nil {
					t.Fatalf("lookup: %v", err)
				}
				if stored.RotatedAt != nil {
					t.Fatalf("a refused rotation was applied: rotated_at = %v", stored.RotatedAt)
				}
			})
		}
	}
}
