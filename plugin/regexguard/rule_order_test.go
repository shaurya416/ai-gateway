package regexguard

import (
	"context"
	"testing"

	"github.com/ferro-labs/ai-gateway/plugin"
)

// A warn rule matching ahead of a block rule does not shadow it: every rule is
// screened, each match is recorded, and the block still denies.
func TestExecute_WarnMatchDoesNotShadowALaterBlockRule(t *testing.T) {
	g := &RegexGuard{}
	if err := g.Init(map[string]any{
		"rules": []any{
			map[string]any{"name": "mention", "pattern": `internal`, "action": "warn"},
			map[string]any{"name": "ssn", "pattern": `\d{3}-\d{2}-\d{4}`, "action": "block"},
		},
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	pctx := newRequest("internal record 123-45-6789")
	if err := g.Execute(context.Background(), pctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !pctx.Reject {
		t.Fatal("a block rule was skipped because a warn rule matched the same text first")
	}
	got := make([]string, 0, len(pctx.GuardrailMatches))
	for _, m := range pctx.GuardrailMatches {
		got = append(got, m.Action)
	}
	if len(got) != 2 || got[0] != plugin.ActionWarn || got[1] != plugin.ActionBlock {
		t.Fatalf("guardrail matches = %v, want [warn block]", got)
	}
}
