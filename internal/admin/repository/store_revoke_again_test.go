package repository

import (
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/internal/admin/model"
)

// A key is revoked once. Revoking it again — a second `ferrogw admin keys
// revoke`, or a second operator acting on a stale page — must not move
// revoked_at to the later call: that timestamp is how an incident review dates
// when a leaked credential stopped working, and a later value reads as the key
// having been live for longer than it was.
func TestStoreRevoke_AgainKeepsTheFirstRevocationTime(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store Store
	}{
		{name: "memory", store: NewKeyStore()},
		{name: "sqlite", store: newSQLiteTestStore(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			created, err := tc.store.Create(ctx, "leaked", []string{model.ScopeReadOnly}, nil)
			if err != nil {
				t.Fatalf("create key: %v", err)
			}
			if err := tc.store.Revoke(ctx, created.ID); err != nil {
				t.Fatalf("first revoke: %v", err)
			}
			first, err := tc.store.Lookup(ctx, created.ID)
			if err != nil || first.RevokedAt == nil {
				t.Fatalf("after first revoke: key %+v, err %v; want revoked_at set", first, err)
			}

			// Far enough apart that any clock the store reads moves.
			time.Sleep(5 * time.Millisecond)

			if err := tc.store.Revoke(ctx, created.ID); err != nil {
				t.Fatalf("second revoke: %v", err)
			}
			second, err := tc.store.Lookup(ctx, created.ID)
			if err != nil || second.RevokedAt == nil {
				t.Fatalf("after second revoke: key %+v, err %v; want revoked_at set", second, err)
			}
			if !second.RevokedAt.Equal(*first.RevokedAt) {
				t.Fatalf("revoked_at moved from %s to %s on a second revoke; it must keep the first",
					first.RevokedAt.Format(time.RFC3339Nano), second.RevokedAt.Format(time.RFC3339Nano))
			}
			if second.Active {
				t.Fatal("a key revoked twice reads as active")
			}
		})
	}
}
