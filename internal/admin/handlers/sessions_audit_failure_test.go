package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/internal/admin/model"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
)

var errSessionStoreDown = errors.New("session store unavailable")

// refusingSessionStore is a working session store whose writes can be made to
// fail, so a test reaches the error path of each session route while the
// request itself still authenticates — with an API key, which never touches
// the session store.
type refusingSessionStore struct {
	repository.SessionStore
	refuse bool
}

func (s *refusingSessionStore) CreateSession(ctx context.Context, subject, credentialID string, scopes []string, ttl time.Duration) (*model.Session, string, error) {
	if s.refuse {
		return nil, "", errSessionStoreDown
	}
	return s.SessionStore.CreateSession(ctx, subject, credentialID, scopes, ttl)
}

func (s *refusingSessionStore) DeleteSession(ctx context.Context, id string) error {
	if s.refuse {
		return errSessionStoreDown
	}
	return s.SessionStore.DeleteSession(ctx, id)
}

func (s *refusingSessionStore) DeleteAllSessions(ctx context.Context) (int, error) {
	if s.refuse {
		return 0, errSessionStoreDown
	}
	return s.SessionStore.DeleteAllSessions(ctx)
}

// TestAudit_SessionStoreRefusalsAreRecorded pins that a sign-in, a sign-out of
// one operator and a sign-out of everyone each leave an error row when the
// session store refuses them, as a refused key, config or purge mutation does.
// Each one wrote only a log line, so the trail could not say that anyone had
// tried to cut an operator off — the question it is read to answer after an
// incident.
func TestAudit_SessionStoreRefusalsAreRecorded(t *testing.T) {
	cases := []struct {
		name, method, path, action, target string
	}{
		{"sign-in", http.MethodPost, "/admin/session", "session.create", ""},
		{"revoke one", http.MethodDelete, "/admin/sessions/0123abcd", "session.revoke", "0123abcd"},
		{"revoke all", http.MethodDelete, "/admin/sessions", "session.revoke_all", "*"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessions := &refusingSessionStore{SessionStore: repository.NewSessionStore(), refuse: true}
			h, r := routerWithStores(repository.NewKeyStore(), sessions, "")
			audit := repository.NewMemoryAuditStore()
			h.Audit = audit
			operator := createAdminKey(t, h)

			w := httptest.NewRecorder()
			r.ServeHTTP(w, authedRequest(tc.method, tc.path, "", operator))
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("%s %s = %d, want 500: %s", tc.method, tc.path, w.Code, w.Body.String())
			}

			entries := auditEntries(t, audit, tc.action)
			if len(entries) != 1 {
				t.Fatalf("got %d %s rows, want 1: a refused %s left no trace in the trail", len(entries), tc.action, tc.name)
			}
			e := entries[0]
			if e.Outcome != model.AuditError {
				t.Errorf("outcome = %q, want error", e.Outcome)
			}
			if e.ActorID != operator.ID {
				t.Errorf("actor_id = %q, want the operator %q", e.ActorID, operator.ID)
			}
			target := tc.target
			if target == "" {
				target = operator.ID
			}
			if e.TargetID != target {
				t.Errorf("target_id = %q, want %q", e.TargetID, target)
			}
		})
	}
}
