package repository

import (
	"context"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
)

// storedLiteralWarning is the line warnStoredLiterals writes.
const storedLiteralWarning = "config value is stored as written but withheld from GET /admin/config"

// The stored-literal warning names a field the read path changed, by asking
// what scrubbing changed. A value an operator wrote that happens to contain a
// redaction token, and that GET /admin/config serves exactly as written, was
// withheld from nobody: pii-redact's own default placeholder, "[REDACTED]",
// spelled out the way config.example.yaml spells it, warned on every apply that
// plugins[0].config was stored as written but withheld.
func TestStoredLiteralWarning_NamesOnlyWhatScrubbingChanged(t *testing.T) {
	piiRedact := config.PluginConfig{
		Name:  "pii-redact",
		Type:  "guardrail",
		Stage: "before_request",
		Config: map[string]any{
			"entities":           []any{"ssn"},
			"action":             "redact",
			"redact_placeholder": "[REDACTED]",
		},
	}
	responseCache := config.PluginConfig{
		Name:  "response-cache",
		Type:  "transform",
		Stage: "before_request",
		Config: map[string]any{
			"backend":   "redis",
			"redis_url": "redis://:literal-password@redis:6379/0",
		},
	}

	exporter := func(apiKey string) []config.ExporterConfig {
		return []config.ExporterConfig{{Name: "langsmith", Config: map[string]any{"api_key": apiKey}}}
	}

	cases := []struct {
		name      string
		plugins   []config.PluginConfig
		exporters []config.ExporterConfig
		wantField string // "" for no warning
	}{
		{"a declared setting served as written", []config.PluginConfig{piiRedact}, nil, ""},
		{"a literal password the read withholds", []config.PluginConfig{piiRedact, responseCache}, nil, "plugins[1].config"},
		{"a literal under a withheld key", []config.PluginConfig{piiRedact}, exporter("literal-key"), "observability.exporters[0].config"},
		{"a reference under a withheld key", []config.PluginConfig{piiRedact}, exporter("${LANGSMITH_API_KEY}"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw, err := newTestGateway(t, baseConfig())
			if err != nil {
				t.Fatalf("new gateway: %v", err)
			}
			mgr, err := NewGatewayConfigManager(gw, &successConfigStore{})
			if err != nil {
				t.Fatalf("new config manager: %v", err)
			}
			cfg := baseConfig()
			cfg.Plugins = tc.plugins
			cfg.Observability.Exporters = tc.exporters

			buf := captureLogger(t)
			if err := mgr.ReloadConfig(context.Background(), cfg); err != nil {
				t.Fatalf("ReloadConfig: %v", err)
			}
			if tc.wantField == "" {
				if strings.Contains(buf.String(), storedLiteralWarning) {
					t.Fatalf("warned about a value GET /admin/config serves as written:\n%s", buf.String())
				}
				return
			}
			if got := findLog(t, buf, storedLiteralWarning)["field"]; got != tc.wantField {
				t.Errorf("field = %v, want %s", got, tc.wantField)
			}
		})
	}
}
