package logger

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ferro-labs/ai-gateway/internal/requestlog"
	"github.com/ferro-labs/ai-gateway/pkg/logger"
	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

// A request can fail after the provider has answered: an after_request plugin
// refuses the response, or an MCP tool loop fails part-way. The provider billed
// that call, and the gateway leaves what it returned on the context. When the
// refusal ends the after_request stage before this plugin writes its row, the
// on_error row is the request's only terminal row, and it recorded the billed
// call as zero tokens and an unknown cost.
func TestRequestLogger_FailureAfterABilledCallKeepsItsUsage(t *testing.T) {
	usage := providers.Usage{PromptTokens: 1200, CompletionTokens: 300, TotalTokens: 1500}
	refused := &plugin.RejectionError{Plugin: "schema-guard", Stage: plugin.StageAfterRequest, Reason: "response is not valid JSON"}

	cases := []struct {
		name       string
		pctx       *plugin.Context
		wantTokens providers.Usage
		wantCost   *float64
	}{
		{
			name: "a response refused after the provider answered",
			pctx: &plugin.Context{
				Request:      &providers.Request{Model: "gpt-4o"},
				Response:     &providers.Response{Model: "gpt-4o", Provider: "openai", Usage: usage},
				Target:       "openai",
				Error:        refused,
				Measurements: plugin.Measurements{DurationMs: 812, CostUSD: 0.006, HasCost: true},
			},
			wantTokens: usage, wantCost: f(0.006),
		},
		{
			// The gateway puts the completed turns' usage on the context and
			// prices none of it on this path, so the tokens are known and the
			// cost is not.
			name: "an MCP tool loop that failed part-way",
			pctx: &plugin.Context{
				Request:      &providers.Request{Model: "gpt-4o"},
				Response:     &providers.Response{Model: "gpt-4o", Provider: "openai", Usage: usage},
				Target:       "openai",
				Error:        errors.New("mcp tool execution at depth 2: tool failed"),
				Measurements: plugin.Measurements{DurationMs: 2400},
			},
			wantTokens: usage, wantCost: nil,
		},
		{
			name: "a provider failure, which was billed nothing",
			pctx: &plugin.Context{
				Request:      &providers.Request{Model: "gpt-4o"},
				Target:       "openai",
				Error:        errors.New("upstream returned 503"),
				Measurements: plugin.Measurements{DurationMs: 90},
			},
			wantTokens: providers.Usage{}, wantCost: nil,
		},
		{
			// A cached response reached no provider, and a refused one was not
			// served either: neither its tokens nor a cost belong to this row.
			name: "a cached response refused at after_request",
			pctx: &plugin.Context{
				Request:      &providers.Request{Model: "gpt-4o"},
				Response:     &providers.Response{Model: "gpt-4o", Provider: "openai", Usage: usage},
				SkipProvider: true,
				Error:        refused,
				Measurements: plugin.Measurements{DurationMs: 2, HasCost: true},
			},
			wantTokens: providers.Usage{}, wantCost: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.pctx.Stage = plugin.StageOnError
			recorder := &recordingWriter{}
			l := &RequestLogger{}
			l.SetRequestLogWriter(recorder)
			if err := l.Init(map[string]any{"persist": true}); err != nil {
				t.Fatalf("init: %v", err)
			}
			if err := l.Execute(context.Background(), tc.pctx); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if len(recorder.entries) != 1 {
				t.Fatalf("expected 1 entry, got %d", len(recorder.entries))
			}
			got := recorder.entries[0]
			if got.Stage != string(plugin.StageOnError) || got.ErrorMessage == "" {
				t.Fatalf("row = %+v, want an on_error row naming the failure", got)
			}
			if got.PromptTokens != tc.wantTokens.PromptTokens ||
				got.CompletionTokens != tc.wantTokens.CompletionTokens ||
				got.TotalTokens != tc.wantTokens.TotalTokens {
				t.Errorf("tokens = %d/%d/%d, want %d/%d/%d", got.PromptTokens, got.CompletionTokens, got.TotalTokens,
					tc.wantTokens.PromptTokens, tc.wantTokens.CompletionTokens, tc.wantTokens.TotalTokens)
			}
			assertFloatPtr(t, "cost_usd", got.CostUSD, tc.wantCost)
		})
	}
}

// The same request read back through the store the dashboard reads: the spend
// and token totals include the call the provider billed before its response
// was refused.
func TestRequestLogger_RefusedResponseCountsTowardTheLogTotals(t *testing.T) {
	store, err := requestlog.NewSQLiteWriter(t.Context(), filepath.Join(t.TempDir(), "requests.db"))
	if err != nil {
		t.Fatalf("new request log store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close request log store: %v", err)
		}
	})

	l := &RequestLogger{}
	l.SetRequestLogWriter(store)
	if err := l.Init(map[string]any{"persist": true}); err != nil {
		t.Fatalf("init: %v", err)
	}

	ctx := logger.WithTraceID(context.Background(), "01960000000000000000000002")
	pctx := plugin.NewContext(&providers.Request{Model: "gpt-4o"})
	t.Cleanup(func() { plugin.PutContext(pctx) })

	// before_request, then a response an after_request guardrail listed ahead
	// of this plugin refused, so its after_request row was never written.
	pctx.Stage = plugin.StageBeforeRequest
	if err := l.Execute(ctx, pctx); err != nil {
		t.Fatalf("before_request: %v", err)
	}
	pctx.Response = &providers.Response{
		Model:    "gpt-4o",
		Provider: "openai",
		Usage:    providers.Usage{PromptTokens: 1200, CompletionTokens: 300, TotalTokens: 1500},
	}
	pctx.Target = "openai"
	pctx.Measurements = plugin.Measurements{DurationMs: 812, CostUSD: 0.006, HasCost: true}
	pctx.Stage = plugin.StageOnError
	pctx.Error = &plugin.RejectionError{Plugin: "secret-scan", Stage: plugin.StageAfterRequest, Reason: "response blocked by content policy: openai_key detected"}
	if err := l.Execute(ctx, pctx); err != nil {
		t.Fatalf("on_error: %v", err)
	}

	stats, err := store.Stats(ctx, requestlog.Query{})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.TotalEntries != 1 || stats.ErrorEntries != 1 {
		t.Fatalf("stats counted %d requests and %d errors, want 1 and 1", stats.TotalEntries, stats.ErrorEntries)
	}
	if stats.TotalTokens != 1500 {
		t.Errorf("total tokens = %d, want 1500: the billed call was recorded as using none", stats.TotalTokens)
	}
	if stats.TotalCostUSD != 0.006 {
		t.Errorf("total cost = %v, want 0.006: the billed call was left out of the spend", stats.TotalCostUSD)
	}
	if stats.UnpricedRequests != 0 {
		t.Errorf("unpriced requests = %d, want 0: the call was priced", stats.UnpricedRequests)
	}
}
