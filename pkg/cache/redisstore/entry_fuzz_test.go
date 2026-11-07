package redisstore

import (
	"encoding/json"
	"reflect"
	"testing"
	"unicode/utf8"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// FuzzEntryRoundTrip holds the entry format to its promise on any response a
// provider can produce: what goes to Redis comes back with every field equal,
// so a hit from Redis is the answer a hit from memory would have been. Text
// fields come from a decoded upstream body, so they are valid UTF-8, and
// metadata holds JSON's own types.
func FuzzEntryRoundTrip(f *testing.F) {
	f.Add("chatcmpl-1", "the answer", "thinking", `{"q":"x"}`, []byte(`{ "content": [] }`), int64(1700000000), 1.25, `{"citations":["a",1.5,true,null]}`)
	f.Add("", "", "", "", []byte(nil), int64(0), 0.0, `null`)
	f.Fuzz(func(t *testing.T, id, content, reasoning, args string, logprobs []byte, created int64, overhead float64, metadata string) {
		for _, s := range []string{id, content, reasoning, args} {
			if !utf8.ValidString(s) {
				return
			}
		}
		var meta map[string]any
		if json.Unmarshal([]byte(metadata), &meta) != nil {
			return
		}
		index := len(content)
		in := &core.Response{
			ID: id, Object: "chat.completion", Created: created, Model: id, Provider: "openai",
			Choices: []core.Choice{{
				Index: index,
				Message: core.Message{
					Role: "assistant", Content: content, ReasoningContent: reasoning,
					ToolCalls: []core.ToolCall{{Index: &index, ID: id, Type: "function", Function: core.FunctionCall{Name: id, Arguments: args}}},
				},
				FinishReason: "stop",
				Logprobs:     core.Logprobs(logprobs),
			}},
			Usage:      core.Usage{PromptTokens: index, CompletionTokens: int(created % 1000), TotalTokens: index + int(created%1000)},
			Metadata:   meta,
			OverheadMs: overhead,
		}
		b, err := encodeEntry(in)
		if err != nil {
			return // a value JSON cannot carry (NaN overhead) is not written
		}
		out, err := decodeEntry(b)
		if err != nil {
			t.Fatalf("decodeEntry of an entry encodeEntry wrote: %v", err)
		}
		if len(in.Choices[0].Logprobs) == 0 {
			// nil and empty logprobs encode differently and both mean "none".
			in.Choices[0].Logprobs, out.Choices[0].Logprobs = nil, nil
		}
		if !reflect.DeepEqual(out, in) {
			t.Fatalf("round trip changed the response\n got: %#v\nwant: %#v", out, in)
		}
	})
}
