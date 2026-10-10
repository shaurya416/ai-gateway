package streamwrap

import (
	"testing"

	"github.com/ferro-labs/ai-gateway/providers"
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
