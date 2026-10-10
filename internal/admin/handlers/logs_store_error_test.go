package handlers

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/internal/admin/model"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
	"github.com/ferro-labs/ai-gateway/internal/requestlog"
	"github.com/ferro-labs/ai-gateway/pkg/logger"
	"github.com/go-chi/chi/v5"
)

// errLogStoreDown is what an unreachable request-log database answers. Its text
// is distinctive so a test can tell whether it reached the server log.
var errLogStoreDown = errors.New("dial tcp 10.9.8.7:5432: connect: connection refused")

// failingLogStore stands in for a request-log database that cannot answer.
type failingLogStore struct{}

func (failingLogStore) List(context.Context, requestlog.Query) (requestlog.ListResult, error) {
	return requestlog.ListResult{}, errLogStoreDown
}

func (failingLogStore) Stats(context.Context, requestlog.Query) (requestlog.StatsResult, error) {
	return requestlog.StatsResult{}, errLogStoreDown
}

func (failingLogStore) Delete(context.Context, requestlog.MaintenanceQuery) (int, error) {
	return 0, errLogStoreDown
}

// A request-log store that failed was answered with a generic 500 and recorded
// nowhere: not in the server log, which every other admin store failure writes
// to, and — for a purge, the one destructive request-log action — not in the
// audit trail, which records a failed key or config mutation. The operator was
// told only "failed to list request logs", and the reason was gone.
func TestRequestLogStoreFailureIsLoggedAndAPurgeAudited(t *testing.T) {
	var buf bytes.Buffer
	original := logger.Default()
	logger.SetDefault(logger.New(logger.Options{Level: "info", Output: &buf}))
	t.Cleanup(func() { logger.SetDefault(original) })

	keys := repository.NewKeyStore()
	audit := repository.NewMemoryAuditStore()
	h := &Handlers{Keys: keys, Logs: failingLogStore{}, LogAdmin: failingLogStore{}, Audit: audit}
	r := chi.NewRouter()
	r.Use(AuthMiddleware(keys, ""))
	r.Mount("/admin", h.Routes())
	adminKey := createAdminKey(t, h)

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/admin/logs"},
		{http.MethodGet, "/admin/logs/stats"},
		{http.MethodGet, "/admin/dashboard"},
		{http.MethodDelete, "/admin/logs?before=2026-01-01T00:00:00Z"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			buf.Reset()
			w := httptest.NewRecorder()
			r.ServeHTTP(w, authedRequest(tc.method, tc.path, "", adminKey))
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
			}
			// The store's error stays out of the response, as it does for the
			// key and config stores.
			if strings.Contains(w.Body.String(), "10.9.8.7") {
				t.Fatalf("the store's error reached the response: %s", w.Body.String())
			}
			if !strings.Contains(buf.String(), errLogStoreDown.Error()) {
				t.Fatalf("the store's error was not logged; log:\n%s", buf.String())
			}
		})
	}

	result, err := audit.List(t.Context(), model.AuditQuery{Action: "logs.purge"})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(result.Data) != 1 {
		t.Fatalf("logs.purge audit rows = %d, want 1 for the failed purge", len(result.Data))
	}
	if got := result.Data[0]; got.Outcome != model.AuditError || got.TargetID != "*" {
		t.Fatalf("logs.purge audit row = %+v, want an error outcome against *", got)
	}
}
