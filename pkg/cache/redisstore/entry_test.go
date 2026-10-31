package redisstore

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// fullResponse sets every field a response can carry, at every depth, to a
// value its zero cannot be mistaken for. TestEntry_FixtureSetsEveryField holds
// it to that, so a field added to core.Response later fails here until the
// fixture — and then the entry format — carries it.
func fullResponse() *core.Response {
	index := 2
	return &core.Response{
		ID:       "chatcmpl-1",
		Object:   "chat.completion",
		Created:  1700000000,
		Model:    "gpt-4o",
		Provider: "openai",
		Choices: []core.Choice{{
			Index: 1,
			Message: core.Message{
				Role: "assistant",
				// Content deliberately differs from the text parts: the client
				// codec rebuilds Content from them, and the cache must not.
				Content: "the answer",
				ContentParts: []core.ContentPart{{
					Type:     "image_url",
					Text:     "a caption",
					ImageURL: &core.ImageURLPart{URL: "https://example.com/a.png", Detail: "high"},
				}},
				Name: "helper",
				ToolCalls: []core.ToolCall{{
					Index:    &index,
					ID:       "call_1",
					Type:     "function",
					Function: core.FunctionCall{Name: "lookup", Arguments: `{"q":"x"}`},
				}},
				ToolCallID:       "call_0",
				ReasoningContent: "thinking",
			},
			FinishReason: "tool_calls",
			// Whitespace kept on purpose: a re-encoded RawMessage is compacted.
			Logprobs: core.Logprobs(`{ "content": [ {"token": "a", "logprob": -0.5} ] }`),
		}},
		// Reasoning outside the completion count, with a total proving it:
		// exactly the shape Usage's own decoder folds into CompletionTokens.
		Usage: core.Usage{
			PromptTokens:     5,
			CompletionTokens: 3,
			TotalTokens:      10,
			ReasoningTokens:  2,
			CacheReadTokens:  4,
			CacheWriteTokens: 1,
		},
		// Metadata is decoded from an upstream body, so it holds JSON's own
		// types: string, float64, bool, nil, []any and map[string]any.
		Metadata: map[string]any{
			"citations": []any{"https://example.com", 1.5, true, nil},
			"search":    map[string]any{"query": "x", "count": 2.0},
		},
		OverheadMs: 1.25,
	}
}

func TestEntry_FixtureSetsEveryField(t *testing.T) {
	t.Parallel()
	requireEveryFieldSet(t, reflect.ValueOf(*fullResponse()), "Response")
}

// requireEveryFieldSet fails for any field under v that holds its zero value:
// a struct field, a slice or map left empty, or a nil pointer.
func requireEveryFieldSet(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			requireEveryFieldSet(t, v.Field(i), path+"."+f.Name)
		}
	case reflect.Slice, reflect.Map:
		if v.Len() == 0 {
			t.Errorf("fullResponse leaves %s empty", path)
			return
		}
		if v.Kind() == reflect.Slice && v.Type().Elem().Kind() != reflect.Uint8 {
			for i := range v.Len() {
				requireEveryFieldSet(t, v.Index(i), path+"[]")
			}
		}
	case reflect.Pointer:
		if v.IsNil() {
			t.Errorf("fullResponse leaves %s nil", path)
			return
		}
		requireEveryFieldSet(t, v.Elem(), path)
	default:
		if v.IsZero() {
			t.Errorf("fullResponse leaves %s at its zero value", path)
		}
	}
}

// A response that has been to Redis and back is the response that went in:
// every field, at every depth, with the same Go types. The memory store hands
// back the very value it was given, so anything less would make a hit from
// Redis a different answer from a hit from memory.
func TestEntry_RoundTripIsLossless(t *testing.T) {
	t.Parallel()

	in := fullResponse()
	b, err := encodeEntry(in)
	if err != nil {
		t.Fatalf("encodeEntry: %v", err)
	}
	out, err := decodeEntry(b)
	if err != nil {
		t.Fatalf("decodeEntry: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip changed the response\n got: %#v\nwant: %#v", out, in)
	}
	// DeepEqual on a named []byte compares contents; say so for the one field
	// most likely to be re-encoded on the way.
	if string(out.Choices[0].Logprobs) != string(in.Choices[0].Logprobs) {
		t.Errorf("logprobs = %s, want %s byte for byte", out.Choices[0].Logprobs, in.Choices[0].Logprobs)
	}
}

// The client encoding is not good enough for this, and the round trip above
// would fail on it. Pinned so a later simplification that reaches for
// json.Marshal(resp) is caught with the reason.
func TestEntry_ClientEncodingWouldLoseFields(t *testing.T) {
	t.Parallel()

	in := fullResponse()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out core.Response
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Usage == in.Usage {
		t.Error("the client codec kept Usage as written; the fold it applies on decode is the reason the entry has its own format")
	}
	if out.OverheadMs == in.OverheadMs {
		t.Error("the client codec kept OverheadMs; it is excluded from client JSON")
	}
	if out.Choices[0].Message.Content == in.Choices[0].Message.Content {
		t.Error("the client codec kept Content beside ContentParts")
	}
}

// nil and empty are different answers on the wire — "choices": null is not
// "choices": [] — so the store keeps them apart wherever the response can hold
// either.
func TestEntry_NilAndEmptyStayDistinct(t *testing.T) {
	t.Parallel()

	for _, in := range []*core.Response{
		{ID: "nil everywhere"},
		{ID: "empty", Choices: []core.Choice{}, Metadata: map[string]any{}},
		{ID: "empty message lists", Choices: []core.Choice{{
			Message:  core.Message{ContentParts: []core.ContentPart{}, ToolCalls: []core.ToolCall{}},
			Logprobs: core.Logprobs{},
		}}},
		// A tool call's index pointing at 0 is set, and stays set.
		{ID: "zero index", Choices: []core.Choice{{
			Message: core.Message{ToolCalls: []core.ToolCall{{Index: new(int)}, {}}},
		}}},
	} {
		b, err := encodeEntry(in)
		if err != nil {
			t.Fatalf("%s: encodeEntry: %v", in.ID, err)
		}
		out, err := decodeEntry(b)
		if err != nil {
			t.Fatalf("%s: decodeEntry: %v", in.ID, err)
		}
		if !reflect.DeepEqual(out, in) {
			t.Errorf("%s: round trip = %#v, want %#v", in.ID, out, in)
		}
		for i := range in.Choices {
			if (in.Choices[i].Logprobs == nil) != (out.Choices[i].Logprobs == nil) {
				t.Errorf("%s: logprobs nil-ness changed", in.ID)
			}
		}
	}
}

func TestDecodeEntry_RefusesWhatItDidNotWrite(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, raw, want string
	}{
		{"not JSON", "plain text someone else stored", "not valid JSON"},
		{"another format version", `{"v":2,"response":{"id":"x"}}`, "format version 2"},
		{"no version", `{"id":"chatcmpl-1","choices":[]}`, "format version 0"},
	} {
		if _, err := decodeEntry([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: decodeEntry error = %v, want one saying %q", tc.name, err, tc.want)
		}
	}
}
