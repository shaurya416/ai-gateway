package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/admin/model"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
)

// errStoreDial is what a config store backed by an unreachable Postgres
// reports: the driver's dial error names the database host and port.
var errStoreDial = errors.New("dial tcp 10.20.30.40:5432: connect: connection refused")

// unreachableConfigStoreManager is a config manager whose persistent store has
// gone away: every operation that has to read or write it fails, wrapping the
// driver error the way GatewayConfigManager and SQLConfigStore do.
type unreachableConfigStoreManager struct {
	testConfigManager
}

func (m *unreachableConfigStoreManager) ReloadConfig(context.Context, config.Config) error {
	return errors.Join(repository.ErrConfigPersistence, fmt.Errorf("save config: %w", errStoreDial))
}

func (m *unreachableConfigStoreManager) ResetConfig(context.Context) error {
	return errors.Join(repository.ErrConfigPersistence, fmt.Errorf("delete config: %w", errStoreDial))
}

func (m *unreachableConfigStoreManager) LoadHistory(context.Context) ([]model.PersistedConfigVersion, bool, error) {
	return nil, true, fmt.Errorf("load config history: %w", errStoreDial)
}

var _ repository.ConfigHistoryLoader = (*unreachableConfigStoreManager)(nil)

// TestConfigStoreFailure_ErrorBodyNamesNoStoreDetail pins that a config store
// that cannot be reached is a 500 whose body says what failed and nothing about
// the store itself.
//
// A store error quotes its database: the host and port it dialled, the user it
// authenticated as, the database name. The key and credential stores already
// log such an error and write a generic message; the config routes wrote it
// into the response, and GET /admin/config/history is served to read_only
// credentials, so an outage handed the database's address to every holder of
// one.
func TestConfigStoreFailure_ErrorBodyNamesNoStoreDetail(t *testing.T) {
	cm := &unreachableConfigStoreManager{}
	cm.cfg = config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "openai"}},
	}
	cm.initial = cm.cfg
	h, r := setupTestRouterWithConfigManager(cm)
	adminKey := createAdminKey(t, h)
	readOnlyKey := createReadOnlyKey(t, h)

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
		key    *model.APIKey
	}{
		{name: "history read", method: http.MethodGet, path: "/admin/config/history", key: readOnlyKey},
		{name: "update", method: http.MethodPut, path: "/admin/config", body: fallbackConfigBody, key: adminKey},
		{name: "create", method: http.MethodPost, path: "/admin/config", body: fallbackConfigBody, key: adminKey},
		{name: "reset", method: http.MethodDelete, path: "/admin/config", key: adminKey},
		{name: "rollback", method: http.MethodPost, path: "/admin/config/rollback/1", key: adminKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, authedRequest(tc.method, tc.path, tc.body, tc.key))

			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
			}
			var payload struct {
				Error struct {
					Message string `json:"message"`
					Code    string `json:"code"`
				} `json:"error"`
			}
			decodeJSON(t, w.Body, &payload)
			if payload.Error.Code != "internal_error" || payload.Error.Message == "" {
				t.Fatalf("error = %+v; want code internal_error and a message", payload.Error)
			}
			for _, leaked := range []string{"10.20.30.40", "5432", "dial tcp", "connection refused"} {
				if strings.Contains(payload.Error.Message, leaked) {
					t.Fatalf("error message %q quotes the store error (%q)", payload.Error.Message, leaked)
				}
			}
		})
	}
}
