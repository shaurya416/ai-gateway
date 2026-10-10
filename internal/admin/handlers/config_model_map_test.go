package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
)

// TestConfigModelMapRoundTrips is the editor's path end to end: a config carrying
// a target's model_map is read back with the map intact, and sending that read
// straight back is accepted.
//
// model_map was withheld as an undeclared free-form map, so GET answered
// {"[REDACTED_KEY_0]": "[REDACTED]"} for it and PUT then refused the body as a
// redacted round-trip — a deployment using model_map could not save any config
// change from the dashboard without retyping every entry.
func TestConfigModelMapRoundTrips(t *testing.T) {
	h, r := setupTestRouter()
	adminKey := createAdminKey(t, h)

	seeded := `{"strategy":{"mode":"single"},"targets":[{"virtual_key":"openai","model_map":{"support-chat":"gpt-4o-mini"}}]}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodPut, "/admin/config", seeded, adminKey))
	if w.Code != http.StatusOK {
		t.Fatalf("seeding config: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	getW := httptest.NewRecorder()
	r.ServeHTTP(getW, authedRequest(http.MethodGet, "/admin/config", "", adminKey))
	if getW.Code != http.StatusOK {
		t.Fatalf("GET /admin/config: expected 200, got %d: %s", getW.Code, getW.Body.String())
	}
	readBack := getW.Body.String()
	var served config.Config
	decodeJSON(t, getW.Body, &served)
	if len(served.Targets) != 1 || served.Targets[0].ModelMap["support-chat"] != "gpt-4o-mini" {
		t.Fatalf("model_map was not served as configured: %s", readBack)
	}

	putW := httptest.NewRecorder()
	r.ServeHTTP(putW, authedRequest(http.MethodPut, "/admin/config", readBack, adminKey))
	if putW.Code != http.StatusOK {
		t.Fatalf("PUT of an unmodified read: expected 200, got %d: %s", putW.Code, putW.Body.String())
	}
}
