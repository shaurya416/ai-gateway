package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	core "github.com/ferro-labs/ai-gateway/providers/core"
)

// openAILogprobs is a choice's logprobs object as OpenAI returns it
// (openai-go ChatCompletionChoiceLogprobs).
const openAILogprobs = `{"content":[{"token":"Hi","logprob":-0.25,"bytes":[72,105],"top_logprobs":[]}],"refusal":null}`

// logprobsOf marshals v as the caller receives it and returns the first
// choice's "logprobs" member, nil when it carries none.
func logprobsOf(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out struct {
		Choices []map[string]json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	if len(out.Choices) == 0 {
		t.Fatalf("no choices in %s", b)
	}
	return out.Choices[0]["logprobs"]
}

func compactJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s is not JSON: %v", raw, err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// TestOpenAICarriesLogprobs pins that the logprobs OpenAI returns for a request
// that asked for them reach the caller, on both surfaces. The request field was
// forwarded verbatim and the answer's was dropped, so logprobs:true was billed
// upstream and served as a 200 with no logprobs at all.
func TestOpenAICarriesLogprobs(t *testing.T) {
	want := compactJSON(t, []byte(openAILogprobs))
	req := core.Request{
		Model:    "gpt-4o-mini",
		LogProbs: true,
		Messages: []core.Message{{Role: core.RoleUser, Content: "hi"}},
	}

	t.Run("complete", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"gpt-4o-mini","choices":[`+
				`{"index":0,"message":{"role":"assistant","content":"Hi"},"logprobs":`+openAILogprobs+`,"finish_reason":"stop"}],`+
				`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		}))
		defer srv.Close()

		p, err := New("sk-test", srv.URL)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		resp, err := p.Complete(context.Background(), req)
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		got := logprobsOf(t, resp)
		if got == nil {
			t.Fatal("response carries no logprobs; OpenAI returned them")
		}
		if g := compactJSON(t, got); g != want {
			t.Errorf("logprobs = %s, want %s", g, want)
		}
	})

	t.Run("stream", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w,
				`data: {"id":"c1","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"logprobs":`+openAILogprobs+`,"finish_reason":null}]}`+"\n\n"+
					`data: {"id":"c1","model":"gpt-4o-mini","choices":[{"index":0,"delta":{},"logprobs":null,"finish_reason":"stop"}]}`+"\n\n"+
					"data: [DONE]\n\n")
		}))
		defer srv.Close()

		p, err := New("sk-test", srv.URL)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ch, err := p.CompleteStream(context.Background(), req)
		if err != nil {
			t.Fatalf("CompleteStream: %v", err)
		}
		var chunks []core.StreamChunk
		for c := range ch {
			if c.Error != nil {
				t.Fatalf("stream error: %v", c.Error)
			}
			chunks = append(chunks, c)
		}
		if len(chunks) != 2 {
			t.Fatalf("chunks = %d, want 2", len(chunks))
		}
		got := logprobsOf(t, chunks[0])
		if got == nil {
			t.Fatal("first chunk carries no logprobs; OpenAI's frame did")
		}
		if g := compactJSON(t, got); g != want {
			t.Errorf("logprobs = %s, want %s", g, want)
		}
		if last := logprobsOf(t, chunks[1]); last != nil {
			t.Errorf("last chunk logprobs = %s, want the member absent for null", last)
		}
	})
}
