package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The gateway URL and key are read the way an operator writes them: a base
// URL with a trailing slash asked for //health and //admin/..., which the
// gateway answers 404, and a key carrying a trailing newline was refused by
// the HTTP client before a request was sent, though serve trims MASTER_KEY and
// accepts it.
func TestAdminClientNormalisesTheGatewayURLAndKey(t *testing.T) {
	const key = "fgw_test-key"
	tests := []struct {
		name      string
		flagURL   string // "{server}" is replaced with the stub's URL
		envURL    string
		flagKey   string
		masterKey string
	}{
		{name: "--gateway-url with a trailing slash", flagURL: "{server}/", flagKey: key},
		{name: "FERROGW_URL with a trailing slash and newline", envURL: "{server}/\n", flagKey: key},
		{name: "MASTER_KEY with a trailing newline", flagURL: "{server}", masterKey: key + "\n"},
		{name: "--api-key with surrounding spaces", flagURL: "{server}", flagKey: "  " + key + " "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath, gotAuth string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
				if r.URL.Path != "/health" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"ok"}`))
			}))
			defer server.Close()

			expand := func(s string) string { return strings.Replace(s, "{server}", server.URL, 1) }
			t.Setenv("FERROGW_URL", expand(tt.envURL))
			t.Setenv("FERROGW_API_KEY", "")
			t.Setenv("MASTER_KEY", tt.masterKey)

			client := NewAdminClient(expand(tt.flagURL), tt.flagKey)
			var health map[string]any
			if err := client.GetHealth(t.Context(), "/health", &health); err != nil {
				t.Fatalf("GetHealth: %v (path %q)", err, gotPath)
			}
			if gotPath != "/health" {
				t.Errorf("request path = %q, want /health", gotPath)
			}
			if want := "Bearer " + key; gotAuth != want {
				t.Errorf("Authorization = %q, want %q", gotAuth, want)
			}
		})
	}
}
