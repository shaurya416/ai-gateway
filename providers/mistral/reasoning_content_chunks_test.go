package mistral

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestMistralProvider_Complete_ThinkingChunks serves the reasoning response
// Mistral's reasoning guide documents (testdata/chat.reasoning.response.json):
// message.content is a list of chunks, the reasoning trace in a "thinking"
// chunk and the answer in a "text" chunk. The answer must reach the caller as
// the message's content string and the trace as reasoning_content — what the
// streaming path already does with the same chunks — not as a content array
// carrying a bare {"type":"thinking"} part whose text was dropped.
func TestMistralProvider_Complete_ThinkingChunks(t *testing.T) {
	payload, err := os.ReadFile("testdata/chat.reasoning.response.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	p, _ := New("test-key", srv.URL)
	resp, err := p.Complete(context.Background(), core.Request{
		Model:    "mistral-small-2603",
		Messages: []core.Message{{Role: core.RoleUser, Content: "How old is John?"}},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	msg := resp.Choices[0].Message
	if !strings.HasPrefix(msg.Content, "Let's denote the ages of the sisters") ||
		!strings.HasSuffix(msg.Content, `John is \(\boxed{22}\) years old.`) {
		t.Errorf("content = %q, want the text chunk's answer", msg.Content)
	}
	if !strings.HasPrefix(msg.ReasoningContent, "Okay, let's try to figure out John's age.") ||
		!strings.HasSuffix(msg.ReasoningContent, "Therefore, John is 22.") {
		t.Errorf("reasoning_content = %q, want the thinking chunk's text", msg.ReasoningContent)
	}

	// What the caller is served: an OpenAI assistant message, content a string.
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var served struct {
		Choices []struct {
			Message struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("unmarshal served response: %v", err)
	}
	var content string
	if err := json.Unmarshal(served.Choices[0].Message.Content, &content); err != nil {
		t.Errorf("served content = %s, want a JSON string", served.Choices[0].Message.Content)
	}
	if content != msg.Content {
		t.Errorf("served content = %q, want %q", content, msg.Content)
	}
	if served.Choices[0].Message.ReasoningContent != msg.ReasoningContent {
		t.Errorf("served reasoning_content = %q, want %q", served.Choices[0].Message.ReasoningContent, msg.ReasoningContent)
	}
}
