package config_test

import (
	"math"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
)

// A running config is kept as JSON: the config-history store persists it, GET
// /admin/config serves it, and PluginSharingKey decides which plugin entries are
// one instance by comparing it. YAML can spell values JSON cannot, and a YAML
// config carrying one validated, then split a response cache listed at two
// stages into two instances — the lookup never saw what the store wrote, so
// the cache never served a hit — and was served by GET /admin/config as an
// empty body.
func TestValidateConfig_RefusesValuesJSONCannotCarry(t *testing.T) {
	cacheAt := func(stage, extra string) string {
		return "  - name: response-cache\n    type: transform\n    stage: " + stage +
			"\n    enabled: true\n    config:\n      max_age: 300\n" + extra
	}
	for _, tc := range []struct {
		name, doc, wantErr string
	}{
		{
			name: "mapping key that is not a string",
			doc: "targets:\n  - virtual_key: openai\nplugins:\n" +
				cacheAt("before_request", "      notes: {2026: raised}\n") +
				cacheAt("after_request", "      notes: {2026: raised}\n"),
			wantErr: "plugins[0].config.notes has a mapping key, 2026, that is not a string",
		},
		{
			name: "mapping key nested in a list",
			doc: "targets:\n  - virtual_key: openai\nplugins:\n" +
				cacheAt("before_request", "      notes: [{true: x}]\n"),
			wantErr: "plugins[0].config.notes[0] has a mapping key, true, that is not a string",
		},
		{
			name: "non-finite number in a plugin config",
			doc: "targets:\n  - virtual_key: openai\nplugins:\n" +
				cacheAt("before_request", "      ratio: .nan\n") +
				cacheAt("after_request", "      ratio: .nan\n"),
			wantErr: "plugins[0].config.ratio is NaN",
		},
		{
			name:    "non-finite weight where the mode ignores weights",
			doc:     "strategy:\n  mode: fallback\ntargets:\n  - virtual_key: openai\n    weight: .inf\n",
			wantErr: "targets[0].weight is +Inf",
		},
		{
			name: "mapping key that is not a string in an exporter config",
			doc: "targets:\n  - virtual_key: openai\nobservability:\n  exporters:\n    - name: x\n" +
				"      enabled: false\n      config:\n        tags: {1: a}\n",
			wantErr: "observability.exporters[0].config.tags has a mapping key, 1, that is not a string",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.LoadConfig(writeTempFile(t, "gateway.yaml", tc.doc))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			err = config.ValidateConfig(*cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateConfig = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

// The same rule holds for a plugin list handed to the gateway without
// ValidateConfig, which is all LoadPlugins asks of it: an entry with no JSON
// form has no sharing key, so it could not be matched with its other stage.
func TestValidatePlugins_RefusesAConfigWithNoJSONForm(t *testing.T) {
	entry := func(stage string) config.PluginConfig {
		return config.PluginConfig{
			Name: "budget", Stage: stage, Enabled: true,
			Config: map[string]any{"store_id": "team", "spend_limit_usd": 10.0, "input_per_m_tokens": 1.0, "note": math.Inf(1)},
		}
	}
	err := config.ValidatePlugins([]config.PluginConfig{entry("before_request"), entry("after_request")})
	if err == nil || !strings.Contains(err.Error(), "plugins[0].config.note is +Inf") {
		t.Fatalf("ValidatePlugins = %v, want the entry with no JSON form refused", err)
	}

	// A config JSON can carry is untouched by the rule.
	ok := entry("before_request")
	delete(ok.Config, "note")
	if err := config.ValidatePlugins([]config.PluginConfig{ok}); err != nil {
		t.Fatalf("ValidatePlugins = %v for a config JSON can carry", err)
	}
}
