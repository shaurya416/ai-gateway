package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	core "github.com/ferro-labs/ai-gateway/providers/core"
	"github.com/ferro-labs/ai-gateway/providers/core/openaicompat"
)

// TestCompleteStream_DecodesDeltasLikeTheSharedReader holds the openai
// provider's own stream reader to the delta rules every other OpenAI-compatible
// stream is read by (core.MessageDelta): reasoning under either spelling, and
// content sent as a list of chunks. A server behind OPENAI_BASE_URL streams
// both — DeepSeek's reasoner writes reasoning_content, Ollama writes
// reasoning, Mistral's reasoning models send list-form content — and the
// non-streaming path of this provider already keeps all three, so a reader that
// takes content as a string alone drops the reasoning and skips the list-form
// frames, delivering a different answer for the same request once it streams.
func TestCompleteStream_DecodesDeltasLikeTheSharedReader(t *testing.T) {
	frames := []string{
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Two plus "},"finish_reason":null}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"reasoning":"two."},"finish_reason":null}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":[{"type":"text","text":"The answer "}]},"finish_reason":null}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"is 4."},"finish_reason":null}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_1","type":"function","function":{"name":"add","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
	}
	var body strings.Builder
	for _, f := range frames {
		body.WriteString("data: " + f + "\n\n")
	}
	body.WriteString("data: [DONE]\n\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body.String())
	}))
	defer srv.Close()

	p, err := New("sk-test", srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, err := p.CompleteStream(context.Background(), core.Request{
		Model:    "m",
		Messages: []core.Message{{Role: core.RoleUser, Content: "What is 2+2?"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	var got []core.StreamChunk
	for c := range ch {
		if c.Error != nil {
			t.Fatalf("stream error: %v", c.Error)
		}
		got = append(got, c)
	}

	// The shared reader's answer for the same frames is the reference.
	var want []core.StreamChunk
	for _, f := range frames {
		c, err := openaicompat.DecodeStreamChunk([]byte(f))
		if err != nil {
			t.Fatalf("shared decoder: %v", err)
		}
		want = append(want, c)
	}

	if len(got) != len(want) {
		t.Fatalf("got %d chunks, want %d (one per frame): a frame was skipped", len(got), len(want))
	}
	for i := range want {
		if len(got[i].Choices) != 1 || len(want[i].Choices) != 1 {
			t.Fatalf("chunk %d: got %d choices, want 1", i, len(got[i].Choices))
		}
		g, w := got[i].Choices[0], want[i].Choices[0]
		if g.Delta.Content != w.Delta.Content || g.Delta.ReasoningContent != w.Delta.ReasoningContent ||
			g.Delta.Role != w.Delta.Role || g.FinishReason != w.FinishReason {
			t.Errorf("chunk %d: delta = {role %q, content %q, reasoning %q, finish %q}, "+
				"the shared reader gives {role %q, content %q, reasoning %q, finish %q}",
				i, g.Delta.Role, g.Delta.Content, g.Delta.ReasoningContent, g.FinishReason,
				w.Delta.Role, w.Delta.Content, w.Delta.ReasoningContent, w.FinishReason)
		}
	}

	last := got[len(got)-1].Choices[0].Delta.ToolCalls
	if len(last) != 1 || last[0].Index == nil || *last[0].Index != 1 ||
		last[0].ID != "call_1" || last[0].Function.Name != "add" || last[0].Function.Arguments != "{}" {
		t.Errorf("tool call = %+v, want index 1, id call_1, add({})", last)
	}
}
