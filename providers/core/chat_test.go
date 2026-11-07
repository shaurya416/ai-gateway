package core

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStreamToolCallDeltaOmitsEmptyMetadata(t *testing.T) {
	index := 0
	chunk := StreamChunk{
		Choices: []StreamChoice{{
			Delta: MessageDelta{
				ToolCalls: []ToolCall{{
					Index: &index,
					Function: FunctionCall{
						Arguments: `{"city"`,
					},
				}},
			},
		}},
	}

	raw, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("marshal stream chunk: %v", err)
	}
	var decoded struct {
		Choices []struct {
			Delta struct {
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal stream chunk: %v", err)
	}
	if len(decoded.Choices) != 1 || len(decoded.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("tool call delta missing from %s", raw)
	}
	body := string(decoded.Choices[0].Delta.ToolCalls[0])
	for _, emptyField := range []string{`"id":""`, `"type":""`, `"name":""`} {
		if strings.Contains(body, emptyField) {
			t.Fatalf("stream tool-call delta leaked empty metadata field %s in %s", emptyField, body)
		}
	}
	if !strings.Contains(body, `"arguments":"{\"city\""`) {
		t.Fatalf("stream tool-call delta dropped arguments: %s", body)
	}
}

// TestMessageUnmarshal_ThinkingChunks pins how an array-form content holding a
// thinking chunk decodes: its text becomes ReasoningContent (unless the message
// also names reasoning_content), it is never kept as a part, and when only text
// parts remain the content is the string of their text.
func TestMessageUnmarshal_ThinkingChunks(t *testing.T) {
	tests := []struct {
		name          string
		in            string
		wantContent   string
		wantReasoning string
		wantParts     []string
	}{
		{
			name:          "thinking then text",
			in:            `{"role":"assistant","content":[{"type":"thinking","thinking":[{"type":"text","text":"Two plus "},{"type":"text","text":"two."}]},{"type":"text","text":"4"}]}`,
			wantContent:   "4",
			wantReasoning: "Two plus two.",
		},
		{
			name:          "reasoning_content wins",
			in:            `{"role":"assistant","reasoning_content":"stated","content":[{"type":"thinking","thinking":[{"type":"text","text":"chunk"}]},{"type":"text","text":"4"}]}`,
			wantContent:   "4",
			wantReasoning: "stated",
		},
		{
			name:          "a non-text part stays a part",
			in:            `{"role":"user","content":[{"type":"thinking","thinking":[{"type":"text","text":"t"}]},{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}`,
			wantContent:   "look",
			wantReasoning: "t",
			wantParts:     []string{"text", "image_url"},
		},
		{
			name:        "no thinking keeps text parts",
			in:          `{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`,
			wantContent: "ab",
			wantParts:   []string{"text", "text"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m Message
			if err := json.Unmarshal([]byte(tt.in), &m); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if m.Content != tt.wantContent {
				t.Errorf("Content = %q, want %q", m.Content, tt.wantContent)
			}
			if m.ReasoningContent != tt.wantReasoning {
				t.Errorf("ReasoningContent = %q, want %q", m.ReasoningContent, tt.wantReasoning)
			}
			gotParts := make([]string, 0, len(m.ContentParts))
			for _, p := range m.ContentParts {
				gotParts = append(gotParts, p.Type)
			}
			if strings.Join(gotParts, ",") != strings.Join(tt.wantParts, ",") {
				t.Errorf("ContentParts types = %v, want %v", gotParts, tt.wantParts)
			}
		})
	}
}
