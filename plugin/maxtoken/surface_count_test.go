package maxtoken

import (
	"context"
	"testing"

	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

// A batch embedding is projected to one message per document, so a
// conversation-turn ceiling must not refuse it. The size limits still apply.
func TestMaxToken_MessageCountIsChatOnly(t *testing.T) {
	m := &MaxToken{}
	if err := m.Init(map[string]any{"max_messages": 10}); err != nil {
		t.Fatalf("init: %v", err)
	}
	msgs := make([]providers.Message, 150)
	for i := range msgs {
		msgs[i] = providers.Message{Role: "user", Content: "doc"}
	}

	for _, tc := range []struct {
		name       string
		surface    string
		wantReject bool
	}{
		{"chat request over the ceiling is refused", "", true},
		{"projected embeddings batch is not", "embeddings", false},
		{"projected image request is not", "images", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := providers.Request{Messages: msgs}
			pctx := &plugin.Context{Request: &req, Metadata: map[string]any{}}
			if tc.surface != "" {
				pctx.Metadata[plugin.MetadataSurface] = tc.surface
			}
			if err := m.Execute(context.Background(), pctx); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if pctx.Reject != tc.wantReject {
				t.Errorf("Reject = %v, want %v (reason %q)", pctx.Reject, tc.wantReject, pctx.Reason)
			}
		})
	}
}

// An embeddings input sent as token IDs projects to no messages, so a length
// cap measured it at zero and approved it however long it was: the cap was
// evaded by one client-side tokenizer call. With max_input_length set this
// plugin reads the content, so content it cannot read is denied — the verdict
// RejectUninspectable gives every content guardrail. With the cap off it reads
// no content and the request is not its to refuse.
func TestMaxToken_UninspectableContentAgainstALengthCap(t *testing.T) {
	for _, tc := range []struct {
		name       string
		config     map[string]any
		stage      plugin.Stage
		wantReject bool
	}{
		{name: "length cap denies what it cannot measure", config: map[string]any{"max_input_length": 100}, stage: plugin.StageBeforeRequest, wantReject: true},
		{name: "no length cap reads no content", config: map[string]any{}, stage: plugin.StageBeforeRequest},
		{name: "after_request has nothing left to withhold", config: map[string]any{"max_input_length": 100}, stage: plugin.StageAfterRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &MaxToken{}
			if err := m.Init(tc.config); err != nil {
				t.Fatalf("init: %v", err)
			}
			// What the gateway hands the stage for `input: [[10121, 1178, ...]]`:
			// the model, no messages, and the fact that content was present.
			pctx := &plugin.Context{
				Request: &providers.Request{Model: "text-embedding-3-small"},
				Stage:   tc.stage,
				Metadata: map[string]any{
					plugin.MetadataSurface:              "embeddings",
					plugin.MetadataUninspectableContent: true,
				},
			}
			if err := m.Execute(context.Background(), pctx); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if pctx.Reject != tc.wantReject {
				t.Fatalf("Reject = %v, want %v (reason %q)", pctx.Reject, tc.wantReject, pctx.Reason)
			}
			if !tc.wantReject {
				return
			}
			if len(pctx.GuardrailMatches) != 1 || pctx.GuardrailMatches[0].Action != plugin.ActionBlock {
				t.Errorf("GuardrailMatches = %+v, want one %q match for the denial", pctx.GuardrailMatches, plugin.ActionBlock)
			}
		})
	}
}
