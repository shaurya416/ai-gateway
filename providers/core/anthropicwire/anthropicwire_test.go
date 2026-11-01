package anthropicwire

import (
	"encoding/json"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestMapTools_InputSchemaIsAlwaysAnObject pins the rule MapTools exists for:
// Anthropic requires input_schema to be a schema object, so a tool that brings
// none gets the empty object schema. "parameters": null — which a client that
// serialises unset fields writes for a function taking no arguments, and an MCP
// server can advertise as its inputSchema — decodes to the four bytes "null",
// which is not empty, and was forwarded as input_schema: null, a body Anthropic
// refuses, so every request offering that tool failed.
func TestMapTools_InputSchemaIsAlwaysAnObject(t *testing.T) {
	const emptyObject = `{"type":"object","properties":{}}`
	tests := []struct {
		name string
		tool string
		want string
	}{
		{"parameters absent", `{"type":"function","function":{"name":"ping"}}`, emptyObject},
		{"parameters null", `{"type":"function","function":{"name":"ping","parameters":null}}`, emptyObject},
		{"parameters given", `{"type":"function","function":{"name":"get","parameters":{"type":"object","properties":{"id":{"type":"string"}}}}}`,
			`{"type":"object","properties":{"id":{"type":"string"}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tool core.Tool
			if err := json.Unmarshal([]byte(tt.tool), &tool); err != nil {
				t.Fatalf("decode tool: %v", err)
			}
			got := MapTools([]core.Tool{tool})
			if len(got) != 1 {
				t.Fatalf("MapTools returned %d tools, want 1", len(got))
			}
			body, err := json.Marshal(got[0])
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			var wire struct {
				InputSchema json.RawMessage `json:"input_schema"`
			}
			_ = json.Unmarshal(body, &wire)
			if string(wire.InputSchema) != tt.want {
				t.Errorf("input_schema = %s, want %s (body %s)", wire.InputSchema, tt.want, body)
			}
		})
	}
}
