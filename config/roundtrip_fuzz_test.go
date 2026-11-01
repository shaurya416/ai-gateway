package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
)

// FuzzConfigRoundTrip holds the two config formats to one schema: a document
// either format accepts and ValidateConfig passes must encode as JSON — the
// form the config-history store persists, GET /admin/config serves, and
// PluginSharingKey keys a plugin instance on — and that JSON must decode
// strictly and validate again. A YAML-only value JSON cannot carry is a config
// that starts and then cannot be stored, served or shared across stages.
func FuzzConfigRoundTrip(f *testing.F) {
	if data, err := os.ReadFile("../config.example.yaml"); err == nil {
		f.Add(data, true)
	}
	if data, err := os.ReadFile("../config.example.json"); err == nil {
		f.Add(data, false)
	}
	f.Add([]byte("targets:\n  - virtual_key: openai\n"), true)
	f.Add([]byte("targets:\n  - virtual_key: openai\nplugins:\n  - name: response-cache\n    type: transform\n    stage: before_request\n    enabled: true\n    config:\n      max_age: 300\n      notes: {a: b}\n"), true)
	f.Add([]byte(`{"targets":[{"virtual_key":"openai"}],"plugins":[{"name":"budget","stage":"before_request","enabled":true,"config":{"store_id":"a","x":[1,{"y":null}]}}]}`), false)

	f.Fuzz(func(t *testing.T, data []byte, yamlDoc bool) {
		ext := ".json"
		if yamlDoc {
			ext = ".yaml"
		}
		path := filepath.Join(t.TempDir(), "config"+ext)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.LoadConfig(path)
		if err != nil || config.ValidateConfig(*cfg) != nil {
			return
		}
		encoded, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("a valid %s config cannot be encoded as JSON: %v", ext, err)
		}
		for i, pc := range cfg.Plugins {
			if _, shareable := config.PluginSharingKey(pc); !shareable {
				t.Fatalf("plugins[%d] of a valid %s config has no sharing key", i, ext)
			}
		}
		var again config.Config
		if err := config.DecodeJSONStrict(encoded, &again); err != nil {
			t.Fatalf("the JSON encoding of a valid %s config does not decode: %v\n%s", ext, err, encoded)
		}
		if err := config.ValidateConfig(again); err != nil {
			t.Fatalf("the JSON encoding of a valid %s config does not validate: %v\n%s", ext, err, encoded)
		}
	})
}
