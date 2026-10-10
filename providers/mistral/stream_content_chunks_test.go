package mistral

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestMistralProvider_CompleteStream_ContentChunks covers Mistral's array form
// of delta.content (string | list of content chunks), which its reasoning
// models stream: thinking in "thinking" chunks, the answer in "text" chunks.
// Such a frame used to fail to decode and be skipped, so the stream delivered
// none of what those frames carried and still ended as a success.
func TestMistralProvider_CompleteStream_ContentChunks(t *testing.T) {
	frames := []string{
		`{"id":"c1","model":"magistral-medium-2509","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{"id":"c1","model":"magistral-medium-2509","choices":[{"index":0,"delta":{"content":[{"type":"thinking","thinking":[{"type":"text","text":"Two plus "}]}]},"finish_reason":null}]}`,
		`{"id":"c1","model":"magistral-medium-2509","choices":[{"index":0,"delta":{"content":[{"type":"thinking","thinking":[{"type":"text","text":"two."}]}]},"finish_reason":null}]}`,
		`{"id":"c1","model":"magistral-medium-2509","choices":[{"index":0,"delta":{"content":[{"type":"text","text":"The answer "}]},"finish_reason":null}]}`,
		`{"id":"c1","model":"magistral-medium-2509","choices":[{"index":0,"delta":{"content":"is "},"finish_reason":null}]}`,
		`{"id":"c1","model":"magistral-medium-2509","choices":[{"index":0,"delta":{"content":[{"type":"text","text":"4."}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":12,"total_tokens":21}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			_, _ = io.WriteString(w, "data: "+f+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p, _ := New("test-key", srv.URL)
	ch, err := p.CompleteStream(context.Background(), core.Request{
		Model:    "magistral-medium-2509",
		Messages: []core.Message{{Role: core.RoleUser, Content: "What is 2+2?"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream() error: %v", err)
	}

	var (
		content, reasoning strings.Builder
		finish             string
		usage              *core.Usage
	)
	for c := range ch {
		if c.Error != nil {
			t.Fatalf("stream error: %v", c.Error)
		}
		for _, choice := range c.Choices {
			content.WriteString(choice.Delta.Content)
			reasoning.WriteString(choice.Delta.ReasoningContent)
			if choice.FinishReason != "" {
				finish = choice.FinishReason
			}
		}
		if c.Usage != nil {
			usage = c.Usage
		}
	}

	if got, want := content.String(), "The answer is 4."; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	if got, want := reasoning.String(), "Two plus two."; got != want {
		t.Errorf("reasoning_content = %q, want %q", got, want)
	}
	if finish != core.FinishReasonStop {
		t.Errorf("finish_reason = %q, want %q (the terminal frame was dropped)", finish, core.FinishReasonStop)
	}
	if usage == nil || usage.TotalTokens != 21 {
		t.Errorf("usage = %+v, want total_tokens 21 (the terminal frame was dropped)", usage)
	}
}
