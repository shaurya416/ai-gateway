package bootstrap

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ferro-labs/ai-gateway/pkg/logger"
)

// TestBuildServerAllowsQuotedCORSOrigins is the regression for a CORS_ORIGINS
// value that arrives with its quotes intact — a compose list entry keeps them,
// which is the spelling deploy/compose.yaml uses. The production wildcard check
// already stripped them, but the allowlist did not, so it held the literal
// `"https://app.example.com"`: an origin no browser sends. Every cross-origin
// request was denied while the gateway reported an allowlist in force.
func TestBuildServerAllowsQuotedCORSOrigins(t *testing.T) {
	const origin = "https://app.example.com"
	for _, value := range []string{
		`"` + origin + `"`,
		`'` + origin + `'`,
		`"https://admin.example.com, ` + origin + `"`,
	} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("GATEWAY_CONFIG", "")
			t.Setenv("CORS_ORIGINS", value)
			t.Setenv("FERRO_MODEL_CATALOG_TIMEOUT", "0")
			t.Setenv("API_KEY_STORE_BACKEND", "memory")
			t.Setenv("CONFIG_STORE_BACKEND", "memory")
			t.Setenv("REQUEST_LOG_STORE_BACKEND", "")
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
			t.Setenv("PORT", "0")

			app, err := buildServer(t.Context(), logger.New(logger.FromEnv()))
			if err != nil {
				t.Fatalf("buildServer: %v", err)
			}
			t.Cleanup(func() {
				if err := closeRuntimeResources(app.resources(), app.otelShutdown); err != nil {
					t.Errorf("close runtime resources: %v", err)
				}
			})

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/livez", nil)
			req.Header.Set("Origin", origin)
			rec := httptest.NewRecorder()
			app.srv.Handler.ServeHTTP(rec, req)

			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
				t.Fatalf("CORS_ORIGINS=%s: Access-Control-Allow-Origin = %q, want %q", value, got, origin)
			}
		})
	}
}
