package streamwrap

import (
	"testing"

	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// contentDelta builds one streaming content fragment for choice index.
func contentDelta(index int, content, finish string) providers.StreamChunk {
	return providers.StreamChunk{Choices: []providers.StreamChoice{{
		Index:        index,
		Delta:        providers.MessageDelta{Content: content},
		FinishReason: finish,
	}}}
}

// TestMeter_ChoiceIndexDoesNotSizeTheResponse guards the response assembled for
// the after_request stage against the choice index an upstream writes. The
// index is an integer off the wire, and the assembly grew its slice up to it:
// one frame naming index 65536 built 65537 choices, and an index in the tens of
// millions — still a single short frame — allocated gigabytes and could take
// the process down for every tenant. Storage now follows the choices a stream
// actually carries.
func TestMeter_ChoiceIndexDoesNotSizeTheResponse(t *testing.T) {
	const far = 1 << 16
	resp := completedResponse(t, contentDelta(far, "far", "stop"))

	if len(resp.Choices) != 1 {
		t.Fatalf("assembled %d choices from a stream that carried one", len(resp.Choices))
	}
	if got := resp.Choices[0]; got.Index != far || got.Message.Content != "far" || got.FinishReason != "stop" {
		t.Fatalf("choice = %+v, want index %d carrying the streamed content", got, far)
	}
}

// TestMeter_AssemblesInterleavedChoicesInIndexOrder pins what the change above
// must keep: n > 1 streams interleave their choices, and may open them out of
// order, yet the response the after_request stage reads lists them by index,
// each whole.
func TestMeter_AssemblesInterleavedChoicesInIndexOrder(t *testing.T) {
	resp := completedResponse(t,
		contentDelta(1, "Bon", ""),
		contentDelta(0, "Hel", ""),
		contentDelta(1, "jour", "stop"),
		contentDelta(0, "lo", "stop"),
	)

	if len(resp.Choices) != 2 {
		t.Fatalf("assembled %d choices, want 2", len(resp.Choices))
	}
	for i, want := range []string{"Hello", "Bonjour"} {
		got := resp.Choices[i]
		if got.Index != i || got.Message.Content != want || got.FinishReason != "stop" || got.Message.Role != "assistant" {
			t.Errorf("choices[%d] = %+v, want index %d with content %q", i, got, i, want)
		}
	}
}

// TestMeter_AssemblesReasoningContent pins the reasoning a reasoning model
// streams beside its answer into the response the after_request stage reads.
// That stage reads it on a unary response — a guardrail screens it and the
// response cache stores it — and the assembly dropped it, so a streamed answer
// reached the stage, and later the cache's replay, without its reasoning.
func TestMeter_AssemblesReasoningContent(t *testing.T) {
	reasoningDelta := func(text string) providers.StreamChunk {
		return providers.StreamChunk{Choices: []providers.StreamChoice{{
			Delta: providers.MessageDelta{ReasoningContent: text},
		}}}
	}
	resp := completedResponse(t,
		reasoningDelta("The user greets; "),
		reasoningDelta("answer in kind."),
		contentDelta(0, "Hello", "stop"),
	)

	if len(resp.Choices) != 1 {
		t.Fatalf("assembled %d choices, want 1", len(resp.Choices))
	}
	got := resp.Choices[0].Message
	if got.ReasoningContent != "The user greets; answer in kind." {
		t.Errorf("reasoning_content = %q, want the streamed reasoning in order", got.ReasoningContent)
	}
	if got.Content != "Hello" {
		t.Errorf("content = %q, want %q", got.Content, "Hello")
	}
}

// TestMeter_AssemblesLogprobs pins each choice's logprobs into the response the
// after_request stage reads and the response cache stores. A streamed choice
// carries the entries for its own delta's tokens; the assembled choice carries
// every token's, as a non-streamed choice does, joined list by list in stream
// order. A member that is null in every chunk stays null, and a choice whose
// fragments are not lists to join is assembled with none rather than with a
// guess.
func TestMeter_AssemblesLogprobs(t *testing.T) {
	withLogprobs := func(index int, content, logprobs string) providers.StreamChunk {
		c := contentDelta(index, content, "")
		c.Choices[0].Logprobs = core.Logprobs(logprobs)
		return c
	}
	resp := completedResponse(t,
		withLogprobs(1, "Bon", `{"content":[{"token":"Bon","logprob":-0.3}],"refusal":null}`),
		withLogprobs(0, "Hel", `{"content":[{"token":"Hel","logprob":-0.1}],"refusal":null}`),
		withLogprobs(2, "x", `-0.5`),
		withLogprobs(1, "jour", `{"content":[{"token":"jour","logprob":-0.4}],"refusal":null}`),
		contentDelta(0, "", "stop"),
		withLogprobs(0, "lo", `{"content":[{"token":"lo","logprob":-0.2}],"refusal":null}`),
		contentDelta(1, "", "stop"),
		contentDelta(2, "", "stop"),
		contentDelta(3, "plain", "stop"),
	)

	if len(resp.Choices) != 4 {
		t.Fatalf("assembled %d choices, want 4", len(resp.Choices))
	}
	want := []string{
		`{"content":[{"token":"Hel","logprob":-0.1},{"token":"lo","logprob":-0.2}],"refusal":null}`,
		`{"content":[{"token":"Bon","logprob":-0.3},{"token":"jour","logprob":-0.4}],"refusal":null}`,
		"",
		"",
	}
	for i, w := range want {
		if got := string(resp.Choices[i].Logprobs); got != w {
			t.Errorf("choices[%d].logprobs = %s, want %s", i, got, w)
		}
	}
}
