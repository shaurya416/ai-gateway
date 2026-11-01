package aigateway

import (
	"context"
	"errors"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/authctx"
	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/plugin/budget"
)

// A budget listed at all three stages, ahead of a guardrail that screens the
// response. The guardrail refuses the answer after the budget's after_request
// stage has recorded the call, and the request then runs on_error carrying the
// same response — where the budget recorded it a second time. One call at
// $0.00105 charged $0.0021, so a $0.002 cap that one call cannot reach closed
// on the first request, and the key's next request was refused before any
// provider call.
func TestPluginChain_BudgetRecordsARefusedResponseOnce(t *testing.T) {
	const storeID = "chain-record-once"
	budgetCfg := map[string]any{
		"store_id":            storeID,
		"spend_limit_usd":     0.002,
		"input_per_m_tokens":  3.0,
		"output_per_m_tokens": 15.0,
	}
	filterCfg := map[string]any{"blocked_words": []any{"hi"}}
	gw, calls := chainGateway(t, []config.PluginConfig{
		{Name: "budget", Type: "ratelimit", Stage: "before_request", Enabled: true, Config: budgetCfg},
		{Name: "budget", Type: "ratelimit", Stage: "after_request", Enabled: true, Config: budgetCfg},
		{Name: "word-filter", Type: "guardrail", Stage: "after_request", Enabled: true, Config: filterCfg},
		{Name: "budget", Type: "ratelimit", Stage: "on_error", Enabled: true, Config: budgetCfg},
	})
	budget.ResetStore(storeID)
	defer budget.ResetStore(storeID)

	ctx := authctx.WithKeyID(context.Background(), "chain-key")
	for i := range 2 {
		_, err := gw.Route(ctx, chainRequest())
		var rejection *plugin.RejectionError
		if !errors.As(err, &rejection) {
			t.Fatalf("request %d: err = %v, want the response refused", i, err)
		}
		if rejection.Plugin != "word-filter" {
			t.Fatalf("request %d refused by %q (%s), want word-filter: one call at $0.00105 was charged against a $0.002 cap twice", i, rejection.Plugin, rejection.Reason)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("provider calls = %d, want 2", got)
	}
}
