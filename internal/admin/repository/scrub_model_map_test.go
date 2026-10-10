package repository

import (
	"maps"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
)

// TestScrubConfigSecrets_ModelMapIsShown holds targets[].model_map to the rule
// aliases already follows. The gateway resolves both maps itself and both halves
// of an entry are model ids — a model_map key is advertised by /v1/models and its
// value is the upstream model every response names in X-Gateway-Model — so there
// is no third party for a credential in it to reach.
//
// Withheld, every entry came back as {"[REDACTED_KEY_0]": "[REDACTED]"}: the
// dashboard's strategy panel, which reads model_map from GET /admin/config,
// could only ever render the placeholders, the config editor refused to save any
// config carrying one, and every apply logged the stored-literal warning for a
// map that holds no credential.
func TestScrubConfigSecrets_ModelMapIsShown(t *testing.T) {
	modelMap := map[string]string{
		"support-chat": "meta-llama/Llama-3.1-8B-Instruct",
		"fast":         "gpt-4o-mini",
	}
	live := config.Config{
		Targets: []config.Target{{VirtualKey: "together", ModelMap: modelMap}},
	}

	got := ScrubConfigSecrets(live)

	if !maps.Equal(got.Targets[0].ModelMap, modelMap) {
		t.Fatalf("model_map = %v, want it served as configured: %v", got.Targets[0].ModelMap, modelMap)
	}
	// Served readable, so the read round-trips: PUT /admin/config refuses a body
	// in which RedactedSecretField finds a token.
	if field := RedactedSecretField(got); field != "" {
		t.Fatalf("RedactedSecretField = %q, want none: a read of this config could not be saved back", field)
	}
	// And nothing here is a stored literal worth warning about.
	if field := RedactedLiteralField(got); field != "" {
		t.Fatalf("RedactedLiteralField = %q, want none: every apply would warn about a credential-free map", field)
	}

	// The served map is a copy: rewriting it cannot reach the live config.
	got.Targets[0].ModelMap["fast"] = "changed"
	if live.Targets[0].ModelMap["fast"] != "gpt-4o-mini" {
		t.Fatalf("scrubbed model_map aliases the live one")
	}
}

// TestScrubConfigSecrets_ModelMapValueRulesStillApply is the counterweight: a
// shown key keeps its shape, but its value still goes through the scalar rules,
// so a credential pasted into model_map by mistake is caught on its shape.
func TestScrubConfigSecrets_ModelMapValueRulesStillApply(t *testing.T) {
	// Built at runtime so the literal does not trip a credential scanner.
	secret := "sk-" + strings.Repeat("x", 24)
	live := config.Config{
		Targets: []config.Target{{VirtualKey: "openai", ModelMap: map[string]string{"fast": secret}}},
	}

	got := ScrubConfigSecrets(live)

	if strings.Contains(got.Targets[0].ModelMap["fast"], secret) {
		t.Fatalf("credential-shaped model_map value survived: %q", got.Targets[0].ModelMap["fast"])
	}
	if field := RedactedSecretField(got); field != "targets[0].model_map" {
		t.Fatalf("RedactedSecretField = %q, want targets[0].model_map", field)
	}
}
