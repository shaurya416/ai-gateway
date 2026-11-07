package maxtoken

import (
	"context"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

// Omitting max_messages keeps the documented default of 100: the hundredth
// message passes and the hundred-and-first is refused.
func TestMaxToken_DefaultMessageLimitIs100(t *testing.T) {
	m := initMaxToken(t, map[string]any{})
	for count, wantReject := range map[int]bool{100: false, 101: true} {
		msgs := make([]string, count)
		for i := range msgs {
			msgs[i] = "m"
		}
		pctx := plugin.NewContext(testRequest("gpt-4", msgs...))
		if err := m.Execute(context.Background(), pctx); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if pctx.Reject != wantReject {
			t.Errorf("%d messages: Reject = %v (%s), want %v", count, pctx.Reject, pctx.Reason, wantReject)
		}
	}
}

// A ceiling equal to the limit does not exceed it.
func TestMaxToken_CeilingEqualToTheLimitPasses(t *testing.T) {
	m := initMaxToken(t, map[string]any{"max_tokens": 100})
	req := testRequest("gpt-4", "hello")
	req.MaxTokens = intPtr(100)
	pctx := plugin.NewContext(req)
	if err := m.Execute(context.Background(), pctx); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if pctx.Reject {
		t.Fatalf("max_tokens equal to the limit was refused: %s", pctx.Reason)
	}
}

// A multipart message is measured from its parts, so text carried in parts
// alone — a message built without a collapsed Content — counts toward
// max_input_length.
func TestMaxToken_MaxInputLengthCountsTextParts(t *testing.T) {
	m := initMaxToken(t, map[string]any{"max_input_length": 10})
	req := &providers.Request{Model: "gpt-4", Messages: []providers.Message{{
		Role: "user",
		ContentParts: []providers.ContentPart{
			{Type: "text", Text: strings.Repeat("a", 8)},
			{Type: "text", Text: strings.Repeat("b", 8)},
		},
	}}}
	pctx := plugin.NewContext(req)
	if err := m.Execute(context.Background(), pctx); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !pctx.Reject {
		t.Fatal("16 bytes of text parts passed a max_input_length of 10")
	}
}

// A context with no request — a stage that carries none — is passed through,
// not dereferenced.
func TestMaxToken_NoRequestIsPassedThrough(t *testing.T) {
	m := initMaxToken(t, map[string]any{"max_input_length": 10})
	pctx := &plugin.Context{Stage: plugin.StageBeforeRequest, Metadata: map[string]any{}}
	if err := m.Execute(context.Background(), pctx); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if pctx.Reject {
		t.Fatalf("a context with no request was refused: %s", pctx.Reason)
	}
}
