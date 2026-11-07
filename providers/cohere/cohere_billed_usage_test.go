package cohere

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// serveCohereFixture answers every request with the named testdata payload.
func serveCohereFixture(t *testing.T, name, contentType string) *httptest.Server {
	t.Helper()
	payload, err := os.ReadFile("testdata/" + name) //nolint:gosec // a fixed testdata filename from this file's callers, never external input.
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCohereProvider_Complete_UsageIsBilledUnits serves the tool-call response
// Cohere's v2 Chat reference documents (testdata/chat.tools.response.json). Its
// usage reports 127 input and 69 output tokens under billed_units, and 1032 and
// 124 under tokens, which counts the prompt template Cohere wraps the request
// in and does not bill. The usage the gateway reports, and prices, is the
// billed one.
func TestCohereProvider_Complete_UsageIsBilledUnits(t *testing.T) {
	srv := serveCohereFixture(t, "chat.tools.response.json", "application/json")

	p, _ := New("test-key", srv.URL)
	resp, err := p.Complete(context.Background(), core.Request{
		Model:    "command-a-plus-05-2026",
		Messages: []core.Message{{Role: core.RoleUser, Content: "Sales summary for 29th September 2023?"}},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	want := core.Usage{PromptTokens: 127, CompletionTokens: 69, TotalTokens: 196}
	if resp.Usage != want {
		t.Errorf("usage = %+v, want the billed units %+v", resp.Usage, want)
	}
	if got := len(resp.Choices[0].Message.ToolCalls); got != 2 {
		t.Errorf("tool calls = %d, want 2", got)
	}
	if got := resp.Choices[0].FinishReason; got != core.FinishReasonToolCalls {
		t.Errorf("finish_reason = %q, want %q", got, core.FinishReasonToolCalls)
	}
}

// TestCohereProvider_CompleteStream_UsageIsBilledUnits serves the streamed
// tool-call example Cohere's v2 Chat reference documents
// (testdata/chat.tools.stream.sse), whose message-end reports 126/84 billed and
// 1589/135 under tokens. The stream reports the billed usage, as the
// non-streaming path does.
func TestCohereProvider_CompleteStream_UsageIsBilledUnits(t *testing.T) {
	srv := serveCohereFixture(t, "chat.tools.stream.sse", "text/event-stream")

	p, _ := New("test-key", srv.URL)
	ch, err := p.CompleteStream(context.Background(), core.Request{
		Model:    "command-a-plus-05-2026",
		Messages: []core.Message{{Role: core.RoleUser, Content: "Sales summary for 29th September 2023?"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream() error: %v", err)
	}
	var usage *core.Usage
	for c := range ch {
		if c.Error != nil {
			t.Fatalf("stream error: %v", c.Error)
		}
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	want := core.Usage{PromptTokens: 126, CompletionTokens: 84, TotalTokens: 210}
	if usage == nil || *usage != want {
		t.Errorf("usage = %+v, want the billed units %+v", usage, want)
	}
}

// TestCohereUsage_FallsBackToTokens pins that a usage object carrying no
// billed_units is still reported from tokens rather than as zero, so a body
// without the billed counts is not recorded as a free request.
func TestCohereUsage_FallsBackToTokens(t *testing.T) {
	tests := []struct {
		name string
		u    cohereUsage
		want core.Usage
	}{
		{
			name: "billed units present",
			u:    cohereUsage{BilledUnits: &cohereTokenCounts{InputTokens: 5, OutputTokens: 418}, Tokens: cohereTokenCounts{InputTokens: 71, OutputTokens: 418}},
			want: core.Usage{PromptTokens: 5, CompletionTokens: 418, TotalTokens: 423},
		},
		{
			name: "billed units without token counts",
			u:    cohereUsage{BilledUnits: &cohereTokenCounts{}, Tokens: cohereTokenCounts{InputTokens: 11, OutputTokens: 7}},
			want: core.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18},
		},
		{
			name: "tokens only",
			u:    cohereUsage{Tokens: cohereTokenCounts{InputTokens: 11, OutputTokens: 7}},
			want: core.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18},
		},
		{
			name: "neither",
			want: core.Usage{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.u.toCore(); got != tt.want {
				t.Errorf("toCore() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
