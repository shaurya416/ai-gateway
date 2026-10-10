package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/admin/model"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
	"github.com/go-chi/chi/v5"
)

// hangUpAfterRevoke commits a revoke and then cancels the request's context:
// the shape of a client that disconnects once the mutation has gone through
// but before the handler has finished answering it.
type hangUpAfterRevoke struct {
	repository.Store
	hangUp context.CancelFunc
}

func (s *hangUpAfterRevoke) Revoke(ctx context.Context, id string) error {
	err := s.Store.Revoke(ctx, id)
	s.hangUp()
	return err
}

// hangUpAfterReload is the config control plane's half of the same shape: the
// config is applied, then the caller is gone.
type hangUpAfterReload struct {
	*testConfigManager
	hangUp context.CancelFunc
}

func (m *hangUpAfterReload) ReloadConfig(ctx context.Context, cfg config.Config) error {
	err := m.testConfigManager.ReloadConfig(ctx, cfg)
	m.hangUp()
	return err
}

// TestAudit_CommittedMutationIsRecordedAfterTheCallerHangsUp pins that the
// durable audit row for a change that has already been applied does not depend
// on the caller still being connected. The append ran on the request context,
// so a SQL audit store refused it with "context canceled" the moment the client
// went away: the credential was revoked, or the config replaced, and the trail
// read to answer "who did that" held nothing.
func TestAudit_CommittedMutationIsRecordedAfterTheCallerHangsUp(t *testing.T) {
	newAudit := func(t *testing.T) *repository.SQLAuditStore {
		t.Helper()
		audit, err := repository.NewSQLiteAuditStore(t.Context(), filepath.Join(t.TempDir(), "audit.db"))
		if err != nil {
			t.Fatalf("open audit store: %v", err)
		}
		t.Cleanup(func() { _ = audit.Close() })
		return audit
	}
	assertRecorded := func(t *testing.T, audit *repository.SQLAuditStore, action, targetID string) {
		t.Helper()
		result, err := audit.List(t.Context(), model.AuditQuery{Action: action})
		if err != nil {
			t.Fatalf("list audit: %v", err)
		}
		if len(result.Data) != 1 {
			t.Fatalf("got %d %s rows, want 1: the change was applied and the trail does not say so", len(result.Data), action)
		}
		if e := result.Data[0]; e.Outcome != model.AuditOK || e.TargetID != targetID {
			t.Errorf("row = {outcome %q, target %q}, want {ok, %q}", e.Outcome, e.TargetID, targetID)
		}
	}

	t.Run("key revoke", func(t *testing.T) {
		audit := newAudit(t)
		keys := repository.NewKeyStore()
		ctx, hangUp := context.WithCancel(t.Context())
		defer hangUp()
		h := &Handlers{Keys: &hangUpAfterRevoke{Store: keys, hangUp: hangUp}, Audit: audit}
		operator := createAdminKey(t, h)
		target := createTestKey(t, h, "departing", []string{model.ScopeAdmin}, nil)
		r := chi.NewRouter()
		r.Use(AuthMiddleware(keys, ""))
		r.Mount("/admin", h.Routes())

		req := authedRequest(http.MethodPost, "/admin/keys/"+target.ID+"/revoke", "", operator).WithContext(ctx)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("revoke status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if got, err := keys.Lookup(t.Context(), target.ID); err != nil || got.Active {
			t.Fatalf("revoke did not commit: key = %+v, err = %v", got, err)
		}
		assertRecorded(t, audit, "key.revoke", target.ID)
	})

	t.Run("config update", func(t *testing.T) {
		audit := newAudit(t)
		keys := repository.NewKeyStore()
		ctx, hangUp := context.WithCancel(t.Context())
		defer hangUp()
		h := &Handlers{
			Keys:    keys,
			Configs: &hangUpAfterReload{testConfigManager: &testConfigManager{}, hangUp: hangUp},
			Audit:   audit,
		}
		operator := createAdminKey(t, h)
		r := chi.NewRouter()
		r.Use(AuthMiddleware(keys, ""))
		r.Mount("/admin", h.Routes())

		req := authedRequest(http.MethodPut, "/admin/config", fallbackConfigBody, operator).WithContext(ctx)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("config update status = %d, want 200: %s", w.Code, w.Body.String())
		}
		assertRecorded(t, audit, auditConfigUpdate, auditConfigTarget)
	})
}
