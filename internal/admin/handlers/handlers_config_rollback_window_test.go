package handlers

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/admin/model"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
	"github.com/go-chi/chi/v5"
)

// The durable trail is read in a bounded window — the newest versions — but it
// is never pruned. A rollback looked only inside that window, so once more
// versions had been applied than it holds — every apply records one — every
// earlier version answered 404 "config version not found" and was recorded as
// a denied rollback, while the row it named was still in the store.
func TestRollbackConfig_ReachesAVersionOlderThanTheHistoryWindow(t *testing.T) {
	ctx := t.Context()
	store, err := repository.NewSQLiteConfigStore(ctx, filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("new config store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Version 1 is the one to restore; every version after it is a different
	// config, and there are more of them than one history read returns.
	original := config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeFallback},
		Targets:  []config.Target{{VirtualKey: "openai"}, {VirtualKey: "anthropic"}},
	}
	later := config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "openai"}},
	}
	if err := store.Save(ctx, original); err != nil {
		t.Fatalf("save version 1: %v", err)
	}
	const laterVersions = maxConfigHistoryEntries + 5
	for range laterVersions {
		if err := store.Save(ctx, later); err != nil {
			t.Fatalf("save later version: %v", err)
		}
	}

	gw, err := newTestGateway(t, later)
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	manager, err := repository.NewGatewayConfigManager(gw, store)
	if err != nil {
		t.Fatalf("new config manager: %v", err)
	}

	keys := repository.NewKeyStore()
	audit := repository.NewMemoryAuditStore()
	h := &Handlers{Keys: keys, Configs: manager, Audit: audit}
	r := chi.NewRouter()
	r.Use(AuthMiddleware(keys, ""))
	r.Mount("/admin", h.Routes())
	adminKey := createAdminKey(t, h)

	// The window really does stop short of version 1, or this proves nothing.
	for _, entry := range getConfigHistoryEntries(t, r, adminKey) {
		if entry.Version == 1 {
			t.Fatal("version 1 is inside the served history window; seed more versions")
		}
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodPost, "/admin/config/rollback/1", "", adminKey))
	if w.Code != http.StatusOK {
		t.Fatalf("rollback to a stored version outside the history window = %d, want 200: %s", w.Code, w.Body.String())
	}

	if got := gw.GetConfig().Strategy.Mode; got != config.ModeFallback {
		t.Fatalf("active mode after rollback = %q, want %q", got, config.ModeFallback)
	}

	// The superseded version is the newest one, wherever the target sat.
	entries := getConfigHistoryEntries(t, r, adminKey)
	newest := entries[len(entries)-1]
	if newest.RolledBackFrom == nil || *newest.RolledBackFrom != laterVersions+1 {
		t.Fatalf("rollback recorded rolled_back_from = %v, want %d", newest.RolledBackFrom, laterVersions+1)
	}
	if newest.Config.Strategy.Mode != config.ModeFallback {
		t.Fatalf("newest version carries mode %q, want the restored %q", newest.Config.Strategy.Mode, config.ModeFallback)
	}

	result, err := audit.List(ctx, model.AuditQuery{Action: auditConfigRollback})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(result.Data) != 1 || result.Data[0].Outcome != model.AuditOK {
		t.Fatalf("rollback audit rows = %+v, want one ok row", result.Data)
	}
}

// A version no store ever recorded is still refused as not found: reaching past
// the window must not turn an unknown version into anything else.
func TestRollbackConfig_UnknownVersionIsStillNotFound(t *testing.T) {
	ctx := t.Context()
	store, err := repository.NewSQLiteConfigStore(ctx, filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("new config store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "openai"}},
	}
	if err := store.Save(ctx, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	gw, err := newTestGateway(t, cfg)
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	manager, err := repository.NewGatewayConfigManager(gw, store)
	if err != nil {
		t.Fatalf("new config manager: %v", err)
	}

	keys := repository.NewKeyStore()
	h := &Handlers{Keys: keys, Configs: manager}
	r := chi.NewRouter()
	r.Use(AuthMiddleware(keys, ""))
	r.Mount("/admin", h.Routes())
	adminKey := createAdminKey(t, h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodPost, "/admin/config/rollback/999", "", adminKey))
	if w.Code != http.StatusNotFound {
		t.Fatalf("rollback to a version never recorded = %d, want 404: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "config version not found") {
		t.Fatalf("body = %s, want the not-found message", w.Body.String())
	}
}
