package repository

import (
	"errors"
	"testing"

	"github.com/ferro-labs/ai-gateway/internal/admin/model"
)

// TestStoreLookup_MissingKeyIsErrKeyNotFound pins Lookup's not-found answer on
// both backends: an error wrapping model.ErrKeyNotFound, which callers map to
// 404, and the key itself when it exists.
func TestStoreLookup_MissingKeyIsErrKeyNotFound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store Store
	}{
		{name: "memory", store: NewKeyStore()},
		{name: "sqlite", store: newSQLiteTestStore(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created, err := tc.store.Create(t.Context(), "lookup-key", []string{model.ScopeAdmin}, nil)
			if err != nil {
				t.Fatalf("create key: %v", err)
			}
			got, err := tc.store.Lookup(t.Context(), created.ID)
			if err != nil || got == nil || got.ID != created.ID {
				t.Fatalf("Lookup(existing) = %v, %v; want the key", got, err)
			}
			if _, err := tc.store.Lookup(t.Context(), "does-not-exist"); !errors.Is(err, model.ErrKeyNotFound) {
				t.Fatalf("Lookup(missing) error = %v, want ErrKeyNotFound", err)
			}
		})
	}
}

// TestSQLStore_Lookup_ReadFailureIsNotNotFound pins the distinction Get cannot
// make: a key read that fails is reported as a failure, never as a key that
// does not exist, while the other statements keep working.
func TestSQLStore_Lookup_ReadFailureIsNotNotFound(t *testing.T) {
	store := newSQLiteTestStore(t)
	created, err := store.Create(t.Context(), "admin", []string{model.ScopeAdmin}, nil)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// Close the prepared by-id statement to simulate a transient read failure.
	if err := store.stmtGetByID.Close(); err != nil {
		t.Fatalf("close stmtGetByID: %v", err)
	}

	_, err = store.Lookup(t.Context(), created.ID)
	if err == nil || errors.Is(err, model.ErrKeyNotFound) {
		t.Fatalf("Lookup on a failed read = %v, want a non-ErrKeyNotFound error", err)
	}
	if count, err := store.CountAdminKeys(t.Context()); err != nil || count != 1 {
		t.Fatalf("CountAdminKeys = %d, %v; want 1, nil (only the key read failed)", count, err)
	}
}
