package budget

import (
	"context"
	"testing"

	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

// The budget store is written once, after the whole request. Inside an agentic
// loop the gateway re-runs this check per turn and stamps what the request has
// spent so far on Measurements — without that term every turn reads the same
// stored figure and the check stops nothing, so a key at 99% of its cap got a
// whole loop however far it ran.
func TestBudget_CountsInRequestSpendFromMeasurements(t *testing.T) {
	const storeID = "turn-spend"
	defer ResetStore(storeID)

	p := &Plugin{}
	if err := p.Init(map[string]any{"spend_limit_usd": 1.0, "input_per_m_tokens": 1.0, "output_per_m_tokens": 2.0, "store_id": storeID}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	newCtx := func(spent float64, priced bool) *plugin.Context {
		return &plugin.Context{
			Stage:        plugin.StageBeforeRequest,
			Request:      &providers.Request{Model: "m"},
			Metadata:     map[string]any{"api_key": "k1"},
			Measurements: plugin.Measurements{CostUSD: spent, HasCost: priced},
		}
	}

	// Nothing stored and nothing spent yet: admitted.
	pctx := newCtx(0, false)
	if err := p.Execute(context.Background(), pctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if pctx.Reject {
		t.Fatalf("rejected with no spend recorded: %s", pctx.Reason)
	}

	// The same key, mid-loop, having already spent past the cap on earlier
	// turns. The store still reads zero, so only Measurements can catch this.
	pctx = newCtx(1.5, true)
	if err := p.Execute(context.Background(), pctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !pctx.Reject {
		t.Fatal("a loop that has already spent past the cap was allowed another turn")
	}

	// An unpriced request carries no cost term and must not be rejected on it.
	// The refusal above recorded the spend it refused on, so the key starts
	// from an empty store again to isolate the cost term.
	ResetStoreKey(storeID, "k1")
	pctx = newCtx(1.5, false)
	if err := p.Execute(context.Background(), pctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if pctx.Reject {
		t.Fatalf("an unpriced request was rejected on a cost it does not have: %s", pctx.Reason)
	}
}

// A denial mid-loop ends the request on its error path, so the after stage —
// where spend is otherwise recorded — never runs for it. The turns already made
// were billed upstream all the same, and leaving them out of the store left a
// key's cap permanently open: each request spent up to the cap, was refused
// with "budget exceeded", and the next one was admitted to spend it again.
func TestBudget_MidLoopDenialRecordsTheSpendItRefusedOn(t *testing.T) {
	const storeID = "turn-denial"
	defer ResetStore(storeID)

	p := &Plugin{}
	if err := p.Init(map[string]any{"spend_limit_usd": 1.0, "input_per_m_tokens": 1.0, "output_per_m_tokens": 2.0, "store_id": storeID}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// A loop turn of a request that has already spent past the cap.
	turn := &plugin.Context{
		Stage:        plugin.StageBeforeRequest,
		Request:      &providers.Request{Model: "m"},
		Metadata:     map[string]any{"api_key": "k1"},
		Measurements: plugin.Measurements{CostUSD: 1.5, HasCost: true},
	}
	if err := p.Execute(context.Background(), turn); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !turn.Reject {
		t.Fatal("a loop that has already spent past the cap was allowed another turn")
	}
	if got := p.store.get("k1"); got != 1.5 {
		t.Fatalf("recorded spend = %v, want 1.5: the turns the refused request already made were billed upstream", got)
	}

	// The key's next request: nothing spent yet in it, so only the store can
	// refuse it.
	next := &plugin.Context{
		Stage:    plugin.StageBeforeRequest,
		Request:  &providers.Request{Model: "m"},
		Metadata: map[string]any{"api_key": "k1"},
	}
	if err := p.Execute(context.Background(), next); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !next.Reject {
		t.Fatal("the key's next request was admitted after its previous one was refused for exceeding the cap")
	}

	// The gateway carries the refused request's usage onto its error path. A
	// budget also listed at on_error must not record that spend a second time.
	turn.Stage = plugin.StageOnError
	turn.Response = &providers.Response{Usage: providers.Usage{PromptTokens: 500_000, CompletionTokens: 500_000}}
	if err := p.Execute(context.Background(), turn); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := p.store.get("k1"); got != 1.5 {
		t.Fatalf("recorded spend = %v after on_error, want 1.5: the refusal already recorded this request", got)
	}
}

// A loop turn that is admitted records nothing: the after stage records the
// whole request at the configured rates once it completes.
func TestBudget_AdmittedLoopTurnRecordsNothing(t *testing.T) {
	const storeID = "turn-admitted"
	defer ResetStore(storeID)

	p := &Plugin{}
	if err := p.Init(map[string]any{"spend_limit_usd": 1.0, "input_per_m_tokens": 1.0, "output_per_m_tokens": 2.0, "store_id": storeID}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	turn := &plugin.Context{
		Stage:        plugin.StageBeforeRequest,
		Request:      &providers.Request{Model: "m"},
		Metadata:     map[string]any{"api_key": "k1"},
		Measurements: plugin.Measurements{CostUSD: 0.5, HasCost: true},
	}
	if err := p.Execute(context.Background(), turn); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if turn.Reject {
		t.Fatalf("a loop under its cap was refused: %s", turn.Reason)
	}
	if got := p.store.get("k1"); got != 0 {
		t.Fatalf("recorded spend = %v, want 0: an admitted turn is recorded by the after stage", got)
	}
}
