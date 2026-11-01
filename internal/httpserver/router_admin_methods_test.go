package httpserver_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
	"github.com/ferro-labs/ai-gateway/internal/httpserver"
	"github.com/ferro-labs/ai-gateway/providers"
)

// A wrong method on an admin route is refused the way it is on every other
// route the gateway serves: a 405 carrying the JSON error envelope, and an
// Allow header naming every method the resource accepts. The admin routes
// answered with chi's bare 405 instead — an empty body, and an Allow that
// omitted HEAD on every GET route, which the router serves HEAD on.
func TestAdminRoutes_WrongMethodIs405WithEnvelopeAndFullAllow(t *testing.T) {
	const masterKey = "sk-admin-methods-master-0123456789"

	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "stub"}},
	})
	if err != nil {
		t.Fatalf("New gateway: %v", err)
	}
	reg := providers.NewRegistry()
	reg.Register(stubProvider{})
	router := httpserver.NewRouter(reg, repository.NewKeyStore(), repository.NewSessionStore(), nil, gw, nil, nil, nil, nil, nil, masterKey, nil)

	tests := []struct {
		method, path string
		allow        []string
	}{
		{http.MethodPut, "/admin/dashboard", []string{http.MethodGet, http.MethodHead}},
		{http.MethodPatch, "/admin/keys", []string{http.MethodGet, http.MethodHead, http.MethodPost}},
		{http.MethodPost, "/admin/keys/some-id", []string{http.MethodDelete, http.MethodGet, http.MethodHead, http.MethodPut}},
		{http.MethodGet, "/admin/session", []string{http.MethodDelete, http.MethodPost}},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+masterKey)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405 (body %s)", w.Code, w.Body.String())
			}
			var allow []string
			for _, value := range w.Header().Values("Allow") {
				for _, method := range strings.Split(value, ",") {
					allow = append(allow, strings.TrimSpace(method))
				}
			}
			slices.Sort(allow)
			if !slices.Equal(allow, tc.allow) {
				t.Errorf("Allow = %v, want %v", allow, tc.allow)
			}
			var envelope struct {
				Error struct {
					Type string `json:"type"`
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != "method_not_supported" {
				t.Errorf("body = %q, want the JSON error envelope with code method_not_supported", w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
		})
	}
}
