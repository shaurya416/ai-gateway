package budget

import (
	"context"
	"testing"

	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

// A budget listed at on_error records a request that failed after the provider
// answered — the after_request stage never reached it — and must not record
// one it already recorded. A request whose response an after_request plugin
// listed AFTER the budget refuses, or which a later after_request plugin fails
// on, runs both stages over the same response: the budget's after_request
// stage recorded its cost, and on_error recorded it again, so the key was
// charged twice for one call and reached its cap at half the spend.
func TestBudget_RecordsARequestOnceAcrossAfterAndOnError(t *testing.T) {
	const storeID = "record-once"
	defer ResetStore(storeID)

	p := makePlugin(t, map[string]any{
		"store_id": storeID, "spend_limit_usd": 10.0,
		"input_per_m_tokens": 1.0, "output_per_m_tokens": 2.0,
	})

	// 1M prompt + 1M completion at $1/M and $2/M is $3.
	pctx := pctxWithKey("k1")
	pctx.Response = &providers.Response{Usage: providers.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000}}

	pctx.Stage = plugin.StageAfterRequest
	if err := p.Execute(context.Background(), pctx); err != nil {
		t.Fatalf("after_request: %v", err)
	}
	if got := p.store.get("k1"); got != 3.0 {
		t.Fatalf("spend after after_request = %v, want 3", got)
	}

	// A later after_request plugin refused the response: the request ends on
	// its error path, carrying the same response.
	pctx.Stage = plugin.StageOnError
	pctx.Error = &plugin.RejectionError{Plugin: "word-filter", Stage: plugin.StageAfterRequest, Reason: "blocked"}
	if err := p.Execute(context.Background(), pctx); err != nil {
		t.Fatalf("on_error: %v", err)
	}
	if got := p.store.get("k1"); got != 3.0 {
		t.Fatalf("spend after on_error = %v, want 3: one call was charged twice", got)
	}

	// A request that never reached after_request is still recorded at
	// on_error: that is what listing the budget there is for.
	failed := pctxWithKey("k2")
	failed.Response = &providers.Response{Usage: providers.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000}}
	failed.Stage = plugin.StageOnError
	failed.Error = &plugin.RejectionError{Plugin: "schema-guard", Stage: plugin.StageAfterRequest, Reason: "not an object"}
	if err := p.Execute(context.Background(), failed); err != nil {
		t.Fatalf("on_error: %v", err)
	}
	if got := p.store.get("k2"); got != 3.0 {
		t.Fatalf("spend recorded at on_error = %v, want 3", got)
	}
}
