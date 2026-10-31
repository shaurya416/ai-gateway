package repository

import (
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
)

// TestScrubConfigSecrets_ResponseCacheRedisURL: redis_url is a declared
// response-cache setting, so GET /admin/config shows it — an operator has to
// see which Redis is configured — but through the URL rule, which withholds
// the password. A ${VAR} reference is served as written, and a served
// placeholder is refused on the way back in.
func TestScrubConfigSecrets_ResponseCacheRedisURL(t *testing.T) {
	plugin := func(redisURL string) config.PluginConfig {
		return config.PluginConfig{
			Name: "response-cache", Type: "transform", Stage: "before_request", Enabled: true,
			Config: map[string]any{
				"backend":       "redis",
				"redis_url":     redisURL,
				"key_prefix":    "prod:",
				"redis_timeout": "250ms",
				"max_age":       300,
			},
		}
	}
	live := config.Config{Plugins: []config.PluginConfig{
		plugin("rediss://gateway:s3cret-pw@cache.internal:6380/2"),
		plugin("redis://:s3cret-pw@cache.internal:6379"),
		plugin("${REDIS_URL}"),
		plugin("redis://:${REDIS_PASSWORD}@cache.internal:6379/0"),
		plugin("redis://cache.internal:6379/0"),
	}}

	got := ScrubConfigSecrets(live).Plugins
	for i, want := range []string{
		"rediss://gateway:" + redactedPlaceholder + "@cache.internal:6380/2",
		"redis://:" + redactedPlaceholder + "@cache.internal:6379",
		"${REDIS_URL}",
		"redis://:${REDIS_PASSWORD}@cache.internal:6379/0",
		"redis://cache.internal:6379/0",
	} {
		if served := got[i].Config["redis_url"]; served != want {
			t.Errorf("plugins[%d] redis_url served as %v, want %q", i, served, want)
		}
		for _, key := range []string{"backend", "key_prefix", "redis_timeout", "max_age"} {
			if got[i].Config[key] != live.Plugins[i].Config[key] {
				t.Errorf("plugins[%d] %s served as %v, want it as written", i, key, got[i].Config[key])
			}
		}
	}
	for i, p := range got {
		if strings.Contains(p.Config["redis_url"].(string), "s3cret-pw") {
			t.Fatalf("plugins[%d] served the password: %v", i, p.Config["redis_url"])
		}
	}
	if live.Plugins[0].Config["redis_url"] != "rediss://gateway:s3cret-pw@cache.internal:6380/2" {
		t.Fatal("live config mutated")
	}

	// A literal password is what the stored-literal warning exists for, and a
	// body round-tripping the placeholder back is refused.
	if field := RedactedLiteralField(ScrubConfigSecrets(live)); field != "plugins[0].config" {
		t.Errorf("RedactedLiteralField = %q, want plugins[0].config", field)
	}
	if field := RedactedSecretField(ScrubConfigSecrets(live)); field != "plugins[0].config" {
		t.Errorf("RedactedSecretField = %q, want plugins[0].config", field)
	}

	// Written with references, the block has nothing to withhold and
	// round-trips through the config editor unchanged.
	referenced := config.Config{Plugins: live.Plugins[2:4]}
	if field := RedactedSecretField(ScrubConfigSecrets(referenced)); field != "" {
		t.Errorf("a block written with references was withheld at %q", field)
	}
}
