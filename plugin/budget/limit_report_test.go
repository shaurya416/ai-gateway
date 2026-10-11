package budget

import (
	"context"
	"testing"

	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

// spend records usd of committed spend for key through the after stage.
func spend(t *testing.T, p *Plugin, key string, promptTokens int) {
	t.Helper()
	pctx := pctxWithKey(key)
	pctx.Stage = plugin.StageAfterRequest
	pctx.Response = &providers.Response{Usage: providers.Usage{PromptTokens: promptTokens}}
	if err := p.Execute(context.Background(), pctx); err != nil {
		t.Fatalf("after stage: %v", err)
	}
	plugin.PutContext(pctx)
}

// check runs the before stage for key and reports whether it was refused.
func check(t *testing.T, p *Plugin, key string) bool {
	t.Helper()
	pctx := pctxWithKey(key)
	defer plugin.PutContext(pctx)
	if err := p.Execute(context.Background(), pctx); err != nil {
		t.Fatalf("before stage: %v", err)
	}
	return pctx.Reject
}

// TestBudget_ReportsFirstRefusalOnly: a key held at its cap by a client that
// keeps retrying is one fact, so the sink hears about the first refusal and
// not the ones after it — until the key is reset and refused again.
func TestBudget_ReportsFirstRefusalOnly(t *testing.T) {
	const storeID = "test-limit-report"
	ResetStore(storeID)
	p := &Plugin{}
	var reports []plugin.LimitExceeded
	p.SetLimitSink(func(le plugin.LimitExceeded) { reports = append(reports, le) })
	if err := p.Init(map[string]any{
		"store_id":           storeID,
		"spend_limit_usd":    1.0,
		"input_per_m_tokens": 1_000_000.0, // $1 per token
	}); err != nil {
		t.Fatal(err)
	}

	if check(t, p, "key-a") {
		t.Fatal("a key with no spend was refused")
	}
	spend(t, p, "key-a", 2) // $2 against a $1 cap

	for range 5 {
		if !check(t, p, "key-a") {
			t.Fatal("a key over its cap was admitted")
		}
	}
	if len(reports) != 1 {
		t.Fatalf("five refusals produced %d reports, want 1", len(reports))
	}
	le := reports[0]
	if le.Plugin != "budget" || le.Kind != plugin.LimitSpend || le.KeyID != "key-a" ||
		le.Scope != storeID || le.Limit != 1.0 || le.Observed != 2.0 {
		t.Fatalf("report = %+v", le)
	}

	// Another key is its own run.
	spend(t, p, "key-b", 3)
	check(t, p, "key-b")
	if len(reports) != 2 || reports[1].KeyID != "key-b" {
		t.Fatalf("reports = %+v, want a second for key-b", reports)
	}

	// A reset admits the key again, so the next time it crosses is a new run.
	ResetStoreKey(storeID, "key-a")
	if check(t, p, "key-a") {
		t.Fatal("a reset key was refused")
	}
	spend(t, p, "key-a", 5)
	check(t, p, "key-a")
	check(t, p, "key-a")
	if len(reports) != 3 || reports[2].KeyID != "key-a" {
		t.Fatalf("reports = %+v, want one more for key-a after its reset", reports)
	}
}

// TestBudget_LimitSinkIsOptional: without a sink a refusal still refuses.
func TestBudget_LimitSinkIsOptional(t *testing.T) {
	const storeID = "test-limit-no-sink"
	ResetStore(storeID)
	p := makePlugin(t, map[string]any{
		"store_id":           storeID,
		"spend_limit_usd":    1.0,
		"input_per_m_tokens": 1_000_000.0,
	})
	spend(t, p, "k", 2)
	if !check(t, p, "k") {
		t.Fatal("over-cap key admitted")
	}
}
