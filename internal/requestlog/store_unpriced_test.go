package requestlog

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Unpriced counts the billed calls whose cost is missing from the spend floor.
// A request that failed before any provider answered — a provider 503, a
// budget or guardrail denial, a model no target serves — was billed nothing,
// and its on_error row carries no tokens and no cost because there was nothing
// to price. Counting it as unpriced reported every failure as spend the
// catalog could not price: a fully priced model with one failed request read
// "1 unpriced", and a key retrying against its spend cap added one per retry.
func TestSQLiteWriter_Stats_UnbilledFailureIsNotUnpriced(t *testing.T) {
	w, err := NewSQLiteWriter(t.Context(), filepath.Join(t.TempDir(), "unpriced.db"))
	if err != nil {
		t.Fatalf("new sqlite writer: %v", err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Errorf("close request log writer: %v", err)
		}
	})

	now := time.Now().UTC()
	cost := 0.01
	for _, e := range []Entry{
		// A priced success.
		{Stage: "after_request", Model: "gpt-4o", Provider: "openai", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, CostUSD: &cost},
		// A provider failure and a budget denial: billed nothing.
		{Stage: "on_error", Model: "gpt-4o", Provider: "openai", ErrorMessage: "upstream returned 503"},
		{Stage: "on_error", Model: "gpt-4o", ErrorMessage: "budget exceeded"},
		// A call the provider answered and billed, refused afterwards, for a
		// model the catalog does not price: tokens and no cost.
		{Stage: "on_error", Model: "local-model", Provider: "vllm", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, ErrorMessage: "response blocked"},
		// A success for a model the catalog does not price.
		{Stage: "after_request", Model: "local-model", Provider: "vllm", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	} {
		e.CreatedAt = now
		if err := w.Write(context.Background(), e); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	got, err := w.Stats(context.Background(), Query{})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if got.UnpricedRequests != 2 {
		t.Errorf("unpriced requests = %d, want 2: only the two billed local-model calls are missing from the spend", got.UnpricedRequests)
	}
	if u := got.ByModel["gpt-4o"].Unpriced; u != 0 {
		t.Errorf("gpt-4o unpriced = %d, want 0: its one billed call was priced, and its failures were billed nothing", u)
	}
	if u := got.ByModel["local-model"].Unpriced; u != 2 {
		t.Errorf("local-model unpriced = %d, want 2", u)
	}
	if u := got.ByProvider["openai"].Unpriced; u != 0 {
		t.Errorf("openai unpriced = %d, want 0", u)
	}
	if u := got.ByStage["on_error"].Unpriced; u != 1 {
		t.Errorf("on_error unpriced = %d, want 1: only the refused call that was billed", u)
	}
}
