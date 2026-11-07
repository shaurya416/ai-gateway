package anthropicwire

import (
	"bytes"
	"encoding/json"
	"testing"
)

// FuzzStreamDecoderSequence feeds one decoder a whole stream of frames, one per
// line, and holds what it emits to the decoder's contract whatever the frames
// say: only an "error" frame ends the stream with an error; tool calls are
// numbered 0, 1, 2… in the order their blocks open; and the terminal usage
// counts the prompt inclusive of its cache reads, with a total that is prompt
// plus completion.
func FuzzStreamDecoderSequence(f *testing.F) {
	f.Add([]byte(`{"type":"message_start","message":{"id":"m","model":"claude","usage":{"input_tokens":5,"cache_read_input_tokens":7,"cache_creation_input_tokens":2}}}
{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}
{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}
{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"a"}}
{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":""}}
{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":1}"}}
{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t2","name":"b"}}
{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`))
	f.Add([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":1}}}
{"type":"error","error":{"type":"overloaded_error","message":"boom"}}`))
	f.Add([]byte(`{"type":"ping"}
{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`))

	f.Fuzz(func(t *testing.T, stream []byte) {
		d := NewStreamDecoder("fuzz", "")
		var prompt, cacheRead, opened int
		for frame := range bytes.SplitSeq(stream, []byte("\n")) {
			var head struct {
				Type string `json:"type"`
			}
			decodable := json.Unmarshal(frame, &head) == nil
			chunks, err := d.Event(frame)
			if err != nil {
				if !decodable || head.Type != "error" {
					t.Fatalf("frame %q ended the stream with %v, but it is not an error frame", frame, err)
				}
				return
			}
			if !decodable {
				if len(chunks) != 0 {
					t.Fatalf("an undecodable frame %q emitted %d chunks", frame, len(chunks))
				}
				continue
			}
			switch head.Type {
			case "message_start":
				// The fields the decoder reads, in the types it reads them as:
				// a frame it cannot decode changes nothing.
				var evt struct {
					Message struct {
						ID    string    `json:"id"`
						Model string    `json:"model"`
						Usage wireUsage `json:"usage"`
					} `json:"message"`
				}
				if json.Unmarshal(frame, &evt) == nil {
					prompt, cacheRead = evt.Message.Usage.InputTokens, evt.Message.Usage.CacheReadInputTokens
				}
			case "content_block_start":
				var evt struct {
					Index        int `json:"index"`
					ContentBlock struct {
						Type string `json:"type"`
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"content_block"`
				}
				if json.Unmarshal(frame, &evt) != nil || evt.ContentBlock.Type != BlockTypeToolUse {
					continue
				}
				if len(chunks) != 1 || len(chunks[0].Choices) != 1 || len(chunks[0].Choices[0].Delta.ToolCalls) != 1 {
					t.Fatalf("a tool_use block opened with %+v", chunks)
				}
				if got := chunks[0].Choices[0].Delta.ToolCalls[0].Index; got == nil || *got != opened {
					t.Fatalf("tool call %d opened with index %v", opened, got)
				}
				opened++
			case "message_delta":
				if len(chunks) == 0 {
					continue
				}
				final := chunks[len(chunks)-1]
				if final.Usage == nil {
					t.Fatalf("the terminal chunk carries no usage: %+v", final)
				}
				if final.Usage.PromptTokens != prompt+cacheRead {
					t.Fatalf("prompt tokens = %d, want input %d + cache reads %d", final.Usage.PromptTokens, prompt, cacheRead)
				}
				if final.Usage.TotalTokens != final.Usage.PromptTokens+final.Usage.CompletionTokens {
					t.Fatalf("total %d is not prompt %d + completion %d", final.Usage.TotalTokens, final.Usage.PromptTokens, final.Usage.CompletionTokens)
				}
			}
		}
	})
}

// wireUsage is Usage as it arrives, before the decoder folds cache reads into
// the input count.
type wireUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}
