package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
	"github.com/go-chi/chi/v5"
)

// TestConfigHistory_MemoryTrailRecordsTheActiveConfig pins that, with no config
// store, the newest version GET /admin/config/history serves is the config the
// gateway runs — the one GET /admin/config serves — rather than the request
// body it was applied from. The manager canonicalises a config before applying
// it (the default apiVersion, a strategy mode written the legacy way or left
// out), so the body and the running config differ exactly there: the history
// showed a version whose mode and apiVersion the gateway had never run, and a
// rollback restored the body rather than what was recorded as active.
func TestConfigHistory_MemoryTrailRecordsTheActiveConfig(t *testing.T) {
	gw, err := newTestGateway(t, singleConfig())
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	manager, err := repository.NewGatewayConfigManager(gw, nil)
	if err != nil {
		t.Fatalf("new config manager: %v", err)
	}
	keys := repository.NewKeyStore()
	h := &Handlers{Keys: keys, Configs: manager}
	r := chi.NewRouter()
	r.Use(AuthMiddleware(keys, ""))
	r.Mount("/admin", h.Routes())
	adminKey := createAdminKey(t, h)

	get := func(path string) []byte {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, authedRequest(http.MethodGet, path, "", adminKey))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	newest := func() []byte {
		t.Helper()
		var payload struct {
			Data []struct {
				Config json.RawMessage `json:"config"`
			} `json:"data"`
		}
		if err := json.Unmarshal(get("/admin/config/history"), &payload); err != nil {
			t.Fatalf("decode history: %v", err)
		}
		if len(payload.Data) == 0 {
			t.Fatal("history is empty")
		}
		return payload.Data[len(payload.Data)-1].Config
	}
	compact := func(raw []byte) string {
		t.Helper()
		var out bytes.Buffer
		if err := json.Compact(&out, raw); err != nil {
			t.Fatalf("compact: %v", err)
		}
		return out.String()
	}

	for _, body := range []string{
		// No apiVersion and no strategy: the gateway runs v1 and single.
		`{"targets":[{"virtual_key":"openai"}]}`,
		// The legacy spelling of load-balance, which the gateway runs canonically.
		`{"apiVersion":"v1","strategy":{"mode":"loadbalance"},"targets":[{"virtual_key":"openai","weight":1}]}`,
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, authedRequest(http.MethodPut, "/admin/config", body, adminKey))
		if w.Code != http.StatusOK {
			t.Fatalf("PUT /admin/config = %d: %s", w.Code, w.Body.String())
		}
		active, recorded := compact(get("/admin/config")), compact(newest())
		if active != recorded {
			t.Errorf("after PUT %s the newest history version is\n  %s\nbut the active config is\n  %s", body, recorded, active)
		}
	}

	// A rollback records what it restored, which is now the active config too.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodPost, "/admin/config/rollback/1", "", adminKey))
	if w.Code != http.StatusOK {
		t.Fatalf("POST rollback = %d: %s", w.Code, w.Body.String())
	}
	if active, recorded := compact(get("/admin/config")), compact(newest()); active != recorded {
		t.Errorf("after rollback the newest history version is\n  %s\nbut the active config is\n  %s", recorded, active)
	}
	if mode := manager.GetConfig().Strategy.Mode; mode != config.ModeSingle {
		t.Errorf("rollback to version 1 runs mode %q, want %q", mode, config.ModeSingle)
	}
}
