package gemini

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// blockedPromptBody is Gemini's answer to a prompt it refuses before generating
// anything: a 200 with promptFeedback.blockReason and no candidates
// (GenerateContentResponse.prompt_feedback and BlockedReason in google-genai).
const blockedPromptBody = `{"promptFeedback":{"blockReason":"SAFETY","safetyRatings":[` +
	`{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","probability":"HIGH","blocked":true}]},` +
	`"usageMetadata":{"promptTokenCount":8,"totalTokenCount":8},` +
	`"modelVersion":"gemini-2.5-flash","responseId":"r-blocked"}`

// TestGeminiProvider_Complete_BlockedPromptIsContentFilter verifies a prompt
// Gemini refuses is answered with one choice finishing content_filter, rather
// than a successful response carrying no choices and no reason.
func TestGeminiProvider_Complete_BlockedPromptIsContentFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, blockedPromptBody)
	}))
	defer srv.Close()

	p, _ := New("test-key", srv.URL)
	resp, err := p.Complete(context.Background(), core.Request{
		Model:    "gemini-2.5-flash",
		Messages: []core.Message{{Role: core.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %#v, want one content_filter choice", resp.Choices)
	}
	choice := resp.Choices[0]
	if choice.FinishReason != core.FinishReasonContentFilter {
		t.Errorf("finish_reason = %q, want %q", choice.FinishReason, core.FinishReasonContentFilter)
	}
	if choice.Message.Role != "assistant" || choice.Message.Content != "" {
		t.Errorf("message = %#v, want an empty assistant message", choice.Message)
	}
	if resp.Usage.PromptTokens != 8 {
		t.Errorf("prompt_tokens = %d, want 8", resp.Usage.PromptTokens)
	}
}

// TestGeminiProvider_CompleteStream_BlockedPromptIsContentFilter verifies the
// streamed refusal — promptFeedback on the first and only chunk — finishes
// content_filter instead of ending cleanly with no finish reason at all.
func TestGeminiProvider_CompleteStream_BlockedPromptIsContentFilter(t *testing.T) {
	chunks := streamGemini(t, "data: "+blockedPromptBody+"\n\n")

	if len(chunks) != 1 {
		t.Fatalf("chunks = %#v, want one terminal chunk", chunks)
	}
	c := chunks[0]
	if c.Error != nil {
		t.Fatalf("error = %v, want a content_filter finish", c.Error)
	}
	if len(c.Choices) != 1 || c.Choices[0].FinishReason != core.FinishReasonContentFilter {
		t.Fatalf("choices = %#v, want one choice finishing %q", c.Choices, core.FinishReasonContentFilter)
	}
	if c.Usage == nil || c.Usage.PromptTokens != 8 {
		t.Errorf("usage = %#v, want prompt_tokens 8", c.Usage)
	}
}
