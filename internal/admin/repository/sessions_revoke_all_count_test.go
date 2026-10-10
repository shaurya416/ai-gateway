package repository

import (
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/internal/sqldb"
)

// TestDeleteAllSessions_CountsOnlyLiveSessions pins what "sign everyone out"
// reports: the operators it actually signed out.
//
// A session past either bound stays stored until the next sign-in sweeps it, and
// DeleteAllSessions counted those rows too. So with one operator signed in and
// another who had closed the tab an hour earlier, GET /admin/sessions listed one
// session, and DELETE /admin/sessions then answered {"revoked": 2} — which the
// dashboard reports as two sessions signed out and the audit trail records as
// count 2, naming a sign-in that no longer existed.
func TestDeleteAllSessions_CountsOnlyLiveSessions(t *testing.T) {
	scopes := []string{"admin"}
	idleSince := time.Now().UTC().Add(-2 * DefaultSessionIdleTTL)

	t.Run("memory", func(t *testing.T) {
		ctx := t.Context()
		store := NewSessionStore()
		if _, _, err := store.CreateSession(ctx, "signed-in", "cred-a", scopes, DefaultSessionTTL); err != nil {
			t.Fatalf("create live session: %v", err)
		}
		gone, _, err := store.CreateSession(ctx, "closed-the-tab", "cred-b", scopes, DefaultSessionTTL)
		if err != nil {
			t.Fatalf("create idle session: %v", err)
		}
		store.mu.Lock()
		store.byID[gone.ID].CreatedAt = idleSince
		store.mu.Unlock()

		assertRevokeAllCount(t, store, 1)
		store.mu.Lock()
		left := len(store.byID) + len(store.byHash)
		store.mu.Unlock()
		if left != 0 {
			t.Errorf("%d entries survived signing everyone out", left)
		}
	})

	for name, tc := range newTestSQLSessionStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			if _, _, err := tc.store.CreateSession(ctx, "signed-in", "cred-a", scopes, DefaultSessionTTL); err != nil {
				t.Fatalf("create live session: %v", err)
			}
			gone, _, err := tc.store.CreateSession(ctx, "closed-the-tab", "cred-b", scopes, DefaultSessionTTL)
			if err != nil {
				t.Fatalf("create idle session: %v", err)
			}
			backdate := sqldb.Bind(tc.dialect, "UPDATE sessions SET created_at = ? WHERE id = ?")
			if _, err := tc.db.ExecContext(ctx, backdate, idleSince, gone.ID); err != nil {
				t.Fatalf("backdate: %v", err)
			}

			assertRevokeAllCount(t, tc.store, 1)
			var left int
			if err := tc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions").Scan(&left); err != nil {
				t.Fatalf("count rows: %v", err)
			}
			if left != 0 {
				t.Errorf("%d rows survived signing everyone out", left)
			}
		})
	}
}

// assertRevokeAllCount checks that the listing and the revoke-all count agree on
// how many operators are signed in.
func assertRevokeAllCount(t *testing.T, store SessionStore, want int) {
	t.Helper()
	listed, err := store.ListSessions(t.Context())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(listed) != want {
		t.Fatalf("ListSessions = %d sessions, want %d", len(listed), want)
	}
	n, err := store.DeleteAllSessions(t.Context())
	if err != nil {
		t.Fatalf("DeleteAllSessions: %v", err)
	}
	if n != want {
		t.Errorf("DeleteAllSessions reported %d sessions signed out; %d was signed in", n, want)
	}
}
