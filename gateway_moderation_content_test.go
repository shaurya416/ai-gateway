package aigateway

import (
	"context"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// mockModerationProvider classifies every input as unflagged and counts calls.
type mockModerationProvider struct {
	mockProvider
	calls int
}

func (m *mockModerationProvider) Moderate(_ context.Context, req providers.ModerationRequest) (*providers.ModerationResponse, error) {
	m.calls++
	return &providers.ModerationResponse{ID: "modr-1", Model: req.Model, Results: []core.ModerationResult{{}}}, nil
}

// A moderation input may be OpenAI's multi-modal array — {"type":"text"} and
// {"type":"image_url"} objects — which is how an image is sent to
// omni-moderation. Its text parts are text, and a content guardrail screens
// them as it screens a string input. The whole array was reported as content
// the gateway could not read instead, so a blocking guardrail refused every
// such request, the text-only ones included, and none of its text was ever
// screened. An image part is not text, exactly as it is not on chat, where a
// message's image is neither screened nor a reason to refuse it.
func TestModerate_ContentGuardrailScreensMultimodalTextParts(t *testing.T) {
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	image := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/cat.png"}}

	tests := []struct {
		name    string
		input   any
		refused bool
	}{
		{name: "clean text part", input: []any{text("say hello")}},
		{name: "clean text part beside an image", input: []any{text("is this safe?"), image}},
		{name: "image part alone", input: []any{image}},
		{name: "blocked word in a text part", input: []any{text("the password is " + blockedWord)}, refused: true},
		{name: "blocked word in a text part beside an image", input: []any{image, text("say " + blockedWord)}, refused: true},
		{name: "blocked word in a string input", input: "say " + blockedWord, refused: true},
		// A part this projection does not know carries content nothing read,
		// so it is still refused as uninspectable.
		{name: "unknown part type", input: []any{text("hello"), map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": "AAAA"}}}, refused: true},
		{name: "text part with no text", input: []any{map[string]any{"type": "text"}}, refused: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mp := &mockModerationProvider{
				mockProvider: mockProvider{name: mockProviderName, models: []string{contentModel}},
			}
			gw := newContentGateway(t, mp, newBlockingWordFilter(t))

			_, err := gw.Moderate(context.Background(), providers.ModerationRequest{
				Model: contentModel,
				Input: tc.input,
			})
			if !tc.refused {
				if err != nil {
					t.Fatalf("Moderate = %v, want it served", err)
				}
				if mp.calls != 1 {
					t.Fatalf("provider called %d times, want 1", mp.calls)
				}
				return
			}
			assertRefused(t, err)
			if mp.calls != 0 {
				t.Errorf("provider called %d times; refused content must never reach it", mp.calls)
			}
		})
	}
}
