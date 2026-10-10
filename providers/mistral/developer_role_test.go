package mistral

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestMistralProvider_DeveloperTurnTravelsAsSystem verifies an OpenAI
// "developer" turn reaches Mistral as a system turn, on both chat surfaces.
// Mistral's message union is discriminated on role and names only system,
// user, assistant and tool, so a developer turn forwarded verbatim refused the
// request. The caller's own request is left as it was: a failover hands the
// same messages to the next target.
func TestMistralProvider_DeveloperTurnTravelsAsSystem(t *testing.T) {
	var bodies []struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}
		bodies = append(bodies, body)
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"mistral-large-latest","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	req := core.Request{
		Model: "mistral-large-latest",
		Messages: []core.Message{
			{Role: core.RoleDeveloper, Content: "Answer tersely."},
			{Role: core.RoleUser, Content: "Hi"},
		},
	}

	p, _ := New("test-key", srv.URL)
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	ch, err := p.CompleteStream(context.Background(), req)
	if err != nil {
		t.Fatalf("CompleteStream() error: %v", err)
	}
	for range ch { //nolint:revive // drain so the upstream request completes before the bodies are read
	}

	if len(bodies) != 2 {
		t.Fatalf("upstream requests = %d, want 2", len(bodies))
	}
	for i, body := range bodies {
		if len(body.Messages) != 2 {
			t.Fatalf("request %d: messages = %d, want 2", i, len(body.Messages))
		}
		if got := body.Messages[0]; got.Role != core.RoleSystem || got.Content != "Answer tersely." {
			t.Errorf("request %d: first message = %+v, want the developer turn as role system", i, got)
		}
		if got := body.Messages[1].Role; got != core.RoleUser {
			t.Errorf("request %d: second message role = %q, want user", i, got)
		}
	}
	if got := req.Messages[0].Role; got != core.RoleDeveloper {
		t.Errorf("caller's request rewritten: first role = %q, want developer", got)
	}
}
