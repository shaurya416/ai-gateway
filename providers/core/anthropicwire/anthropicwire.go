// Package anthropicwire holds request types shared by the standalone Anthropic
// provider and the Anthropic-on-Bedrock path, which target the same Messages API
// JSON shape over different transports.
package anthropicwire

import (
	"bytes"
	"encoding/json"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// Tool is an Anthropic tool definition ({name, description, input_schema}).
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// MapTools converts canonical tool definitions to Anthropic's native shape.
// An empty or null parameter schema defaults to an empty object, which
// Anthropic requires (input_schema is mandatory and must be an object).
func MapTools(tools []core.Tool) []Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		schema := t.Function.Parameters
		// "parameters": null decodes to the bytes of the literal, so it is
		// not empty, and forwarded as written it is an input_schema of null.
		if trimmed := bytes.TrimSpace(schema); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}
	return out
}
