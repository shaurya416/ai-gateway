package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// captureGeminiBody runs one Complete against a stub and returns the request body.
func captureGeminiBody(t *testing.T, req core.Request) map[string]json.RawMessage {
	t.Helper()
	var captured map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
	}))
	defer srv.Close()
	p, _ := New("test-key", srv.URL)
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return captured
}

// TestComplete_SanitizesToolSchema verifies #139 follow-up: JSON-schema keywords
// Gemini's OpenAPI subset rejects (e.g. additionalProperties from OpenAI
// strict-mode tools, $schema) are stripped before forwarding.
func TestComplete_SanitizesToolSchema(t *testing.T) {
	captured := captureGeminiBody(t, core.Request{
		Model:    "gemini-1.5-pro",
		Messages: []core.Message{{Role: core.RoleUser, Content: "hi"}},
		Tools: []core.Tool{{
			Type: "function",
			Function: core.Function{
				Name:       "lookup",
				Parameters: json.RawMessage(`{"type":"object","$schema":"http://json-schema.org/draft-07/schema#","additionalProperties":false,"properties":{"city":{"type":"string","additionalProperties":false}}}`),
			},
		}},
	})

	var body struct {
		Tools []struct {
			FunctionDeclarations []struct {
				Parameters map[string]json.RawMessage `json:"parameters"`
			} `json:"functionDeclarations"`
		} `json:"tools"`
	}
	raw, _ := json.Marshal(captured)
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	params := body.Tools[0].FunctionDeclarations[0].Parameters
	if _, ok := params["additionalProperties"]; ok {
		t.Errorf("additionalProperties must be stripped, got %v", params)
	}
	if _, ok := params["$schema"]; ok {
		t.Errorf("$schema must be stripped, got %v", params)
	}
	// Nested additionalProperties must be stripped too.
	if props, ok := params["properties"]; ok {
		var nested map[string]map[string]json.RawMessage
		_ = json.Unmarshal(props, &nested)
		if _, ok := nested["city"]["additionalProperties"]; ok {
			t.Errorf("nested additionalProperties must be stripped, got %v", nested)
		}
	}
	// Legitimate keywords survive.
	if _, ok := params["type"]; !ok {
		t.Errorf("type must be preserved, got %v", params)
	}
}

// TestComplete_CoalescesParallelToolResults verifies #139 follow-up: parallel
// tool results (each its own role="tool" message) are merged into a single
// user content with multiple functionResponse parts, preserving Gemini's
// strict user/model alternation.
func TestComplete_CoalescesParallelToolResults(t *testing.T) {
	captured := captureGeminiBody(t, core.Request{
		Model: "gemini-2.0-flash",
		Messages: []core.Message{
			{Role: core.RoleUser, Content: "weather + time?"},
			{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{
				{ID: "c1", Type: "function", Function: core.FunctionCall{Name: "weather", Arguments: `{"city":"SF"}`}},
				{ID: "c2", Type: "function", Function: core.FunctionCall{Name: "clock", Arguments: `{"city":"SF"}`}},
			}},
			{Role: core.RoleTool, ToolCallID: "c1", Content: `{"temp":72}`},
			{Role: core.RoleTool, ToolCallID: "c2", Content: `{"time":"3pm"}`},
		},
	})

	var body struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				FunctionResponse *struct {
					Name string `json:"name"`
				} `json:"functionResponse"`
			} `json:"parts"`
		} `json:"contents"`
	}
	raw, _ := json.Marshal(captured)
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	// Expect: [user "weather+time?", model (2 functionCall), user (2 functionResponse)]
	if len(body.Contents) != 3 {
		t.Fatalf("contents len = %d, want 3 (user, model, merged-user); contents=%+v", len(body.Contents), body.Contents)
	}
	last := body.Contents[2]
	if last.Role != core.RoleUser {
		t.Fatalf("merged tool-result role = %q, want user", last.Role)
	}
	var names []string
	for _, p := range last.Parts {
		if p.FunctionResponse != nil {
			names = append(names, p.FunctionResponse.Name)
		}
	}
	if len(names) != 2 || names[0] != "weather" || names[1] != "clock" {
		t.Errorf("merged functionResponse names = %v, want [weather clock]", names)
	}
}

// TestComplete_KeepsPropertiesNamedLikeStrippedKeywords pins that the schema
// sanitizer strips keywords, not the caller's property names. A property is
// named by a key under "properties", and "definitions" or
// "additionalProperties" there is a field of the caller's object, not the
// keyword Gemini rejects. Stripping it deleted the field from the schema the
// model was given while "required" still named it, so a tool parameter or a
// structured-output field never reached Gemini.
func TestComplete_KeepsPropertiesNamedLikeStrippedKeywords(t *testing.T) {
	const schema = `{"type":"object","additionalProperties":false,` +
		`"properties":{"term":{"type":"string"},` +
		`"definitions":{"type":"array","items":{"type":"string"},"additionalProperties":false},` +
		`"additionalProperties":{"type":"string"}},` +
		`"required":["term","definitions","additionalProperties"]}`

	captured := captureGeminiBody(t, core.Request{
		Model:    "gemini-2.5-flash",
		Messages: []core.Message{{Role: core.RoleUser, Content: "define llama"}},
		Tools: []core.Tool{{
			Type:     "function",
			Function: core.Function{Name: "save_glossary", Parameters: json.RawMessage(schema)},
		}},
		ResponseFormat: &core.ResponseFormat{
			Type:       "json_schema",
			JSONSchema: json.RawMessage(`{"name":"glossary","strict":true,"schema":` + schema + `}`),
		},
	})

	var body struct {
		Tools []struct {
			FunctionDeclarations []struct {
				Parameters json.RawMessage `json:"parameters"`
			} `json:"functionDeclarations"`
		} `json:"tools"`
		GenerationConfig struct {
			ResponseSchema json.RawMessage `json:"responseSchema"`
		} `json:"generationConfig"`
	}
	raw, _ := json.Marshal(captured)
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	for where, got := range map[string]json.RawMessage{
		"tool parameters": body.Tools[0].FunctionDeclarations[0].Parameters,
		"responseSchema":  body.GenerationConfig.ResponseSchema,
	} {
		var s struct {
			Properties map[string]map[string]any `json:"properties"`
			Required   []string                  `json:"required"`
		}
		if err := json.Unmarshal(got, &s); err != nil {
			t.Fatalf("%s: %s is not a schema object: %v", where, got, err)
		}
		var top map[string]json.RawMessage
		_ = json.Unmarshal(got, &top)
		if _, ok := top["additionalProperties"]; ok {
			t.Errorf("%s: the additionalProperties keyword must still be stripped: %s", where, got)
		}
		for _, name := range s.Required {
			if _, ok := s.Properties[name]; !ok {
				t.Errorf("%s: required names %q but properties lost it: %s", where, name, got)
			}
		}
		if defs := s.Properties["definitions"]; defs != nil {
			if _, ok := defs["additionalProperties"]; ok {
				t.Errorf("%s: the keyword nested inside a property must still be stripped: %s", where, got)
			}
			if defs["type"] != "array" {
				t.Errorf("%s: properties.definitions.type = %v, want array: %s", where, defs["type"], got)
			}
		}
	}
}
