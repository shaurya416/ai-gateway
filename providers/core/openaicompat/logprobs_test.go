package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// upstreamLogprobs is a choice's logprobs object in the shape OpenAI's chat
// completions return it (openai-go ChatCompletionChoiceLogprobs: content and
// refusal lists of ChatCompletionTokenLogprob).
const upstreamLogprobs = `{"content":[{"token":"Hi","logprob":-0.25,"bytes":[72,105],"top_logprobs":[{"token":"Hi","logprob":-0.25,"bytes":[72,105]}]}],"refusal":null}`

// choiceLogprobs marshals v as the caller would receive it and returns each
// choice's "logprobs" member, nil where the choice carries none.
func choiceLogprobs(t *testing.T, v any) []json.RawMessage {
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
	got := make([]json.RawMessage, len(out.Choices))
	for i, c := range out.Choices {
		got[i] = c["logprobs"]
	}
	return got
}

// assertSameJSON fails unless got and want encode the same JSON value.
func assertSameJSON(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("logprobs = %s, not JSON: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want: %v", err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Errorf("logprobs = %s, want %s", gb, wb)
	}
}

// TestPostChatCarriesLogprobs pins that a choice's logprobs reach the caller.
// The request's logprobs travel upstream, so a choice that came back with them
// and was answered without them handed the caller a 200 indistinguishable from
// a model that returned none. A null logprobs — what OpenAI sends on every
// choice when none were asked for — stays off the response.
func TestPostChatCarriesLogprobs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","model":"m","choices":[`+
			`{"index":0,"message":{"role":"assistant","content":"Hi"},"logprobs":`+upstreamLogprobs+`,"finish_reason":"stop"},`+
			`{"index":1,"message":{"role":"assistant","content":"Hi"},"logprobs":null,"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	resp, err := PostChat(context.Background(), ChatParams{
		HTTPClient: srv.Client(),
		URL:        srv.URL,
		Provider:   "test",
		Label:      "test",
	}, core.Request{Model: "m", LogProbs: true, Messages: []core.Message{{Role: core.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("PostChat: %v", err)
	}

	got := choiceLogprobs(t, resp)
	if len(got) != 2 {
		t.Fatalf("choices = %d, want 2", len(got))
	}
	if got[0] == nil {
		t.Fatal("choice 0 carries no logprobs; the upstream returned them")
	}
	assertSameJSON(t, got[0], upstreamLogprobs)
	if got[1] != nil {
		t.Errorf("choice 1 logprobs = %s, want the member absent for an upstream null", got[1])
	}
}

// TestPostStreamCarriesLogprobs is TestPostChatCarriesLogprobs for a stream:
// each chunk's per-choice logprobs reach the caller, and a null one does not
// add a member to every chunk.
func TestPostStreamCarriesLogprobs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"logprobs":`+upstreamLogprobs+`}]}`+"\n\n"+
				`data: {"id":"c1","choices":[{"index":0,"delta":{},"logprobs":null,"finish_reason":"stop"}]}`+"\n\n"+
				"data: [DONE]\n\n")
	}))
	defer srv.Close()

	ch, err := PostStream(context.Background(), ChatParams{
		HTTPClient: srv.Client(),
		URL:        srv.URL,
		Provider:   "test",
		Label:      "test",
	}, core.Request{Model: "m", LogProbs: true, Messages: []core.Message{{Role: core.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("PostStream: %v", err)
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

	first := choiceLogprobs(t, chunks[0])
	if len(first) != 1 || first[0] == nil {
		t.Fatalf("first chunk logprobs = %v; the upstream frame carried them", first)
	}
	assertSameJSON(t, first[0], upstreamLogprobs)
	if last := choiceLogprobs(t, chunks[1]); len(last) != 1 || last[0] != nil {
		t.Errorf("last chunk logprobs = %v, want the member absent for an upstream null", last)
	}
}
