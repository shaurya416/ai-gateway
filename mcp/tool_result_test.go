package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// wantStructured is helperStructured as the model should read it: the object,
// compacted.
const wantStructured = `{"city":"Oslo","temp_c":21.5}`

// A tool with an output schema may answer in structuredContent alone — the
// spec only asks it to repeat the object as text, and the official TypeScript
// SDK sends such a tool's result exactly that way. Decoding only the content
// blocks handed the model an empty string, recorded as a successful call.
func TestToolResultUsesStructuredContentOverHTTP(t *testing.T) {
	reg := registryAnswering(t, "get_weather", json.RawMessage(
		`{"content":[],"structuredContent":{"city": "Oslo", "temp_c": 21.5}}`,
	))

	msgs, err := NewExecutor(reg, 5, nil).ResolvePendingToolCalls(context.Background(), toolCallResponse("get_weather"))
	if err != nil {
		t.Fatalf("ResolvePendingToolCalls: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want the assistant turn and one tool result", len(msgs))
	}
	if got := msgs[1].Content; got != wantStructured {
		t.Fatalf("tool result handed to the model = %q, want the structured content %s", got, wantStructured)
	}
}

// The stdio transport converts the library's typed result through the same
// struct, so it dropped the same field.
func TestToolResultUsesStructuredContentOverStdio(t *testing.T) {
	client := startHelperClient(t, "structured", helperModeServe)
	reg := registryWith(map[string]mcpClient{"structured": client})
	reg.mu.Lock()
	reg.servers["structured"].tools = []Tool{{Name: helperToolStructured}}
	reg.toolMap[helperToolStructured] = "structured"
	reg.mu.Unlock()

	msg := NewExecutor(reg, 5, nil).executeToolCall(t.Context(), toolCallNamed(helperToolStructured))
	if msg.Content != wantStructured {
		t.Fatalf("tool result handed to the model = %q, want the structured content %s", msg.Content, wantStructured)
	}
}

// When a result carries both, the content blocks are the answer: they are the
// text rendering the spec asks for, and repeating the object beside them would
// hand the model the same data twice.
func TestToolResultPrefersContentBlocksOverStructuredContent(t *testing.T) {
	reg := registryAnswering(t, "get_weather", json.RawMessage(
		`{"content":[{"type":"text","text":"Oslo is 21.5C"}],"structuredContent":{"city":"Oslo","temp_c":21.5}}`,
	))

	msgs, err := NewExecutor(reg, 5, nil).ResolvePendingToolCalls(context.Background(), toolCallResponse("get_weather"))
	if err != nil {
		t.Fatalf("ResolvePendingToolCalls: %v", err)
	}
	if got := msgs[1].Content; got != "Oslo is 21.5C" {
		t.Fatalf("tool result handed to the model = %q, want the text block alone", got)
	}
}

// A tool may report failure with isError and no content at all. Rendered as an
// empty string, the failure reached the model as a successful call that
// returned nothing, and the audit record and span named no reason.
func TestToolErrorWithoutContentReadsAsFailure(t *testing.T) {
	reg := registryAnswering(t, "fail_quietly", json.RawMessage(`{"content":[],"isError":true}`))

	audited := make(chan string, 1)
	audit := func(_ context.Context, _, _, status string, _ int, errMsg string) {
		if status == "error" {
			audited <- errMsg
		}
	}

	msgs, err := NewExecutor(reg, 5, audit).ResolvePendingToolCalls(context.Background(), toolCallResponse("fail_quietly"))
	if err != nil {
		t.Fatalf("ResolvePendingToolCalls: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(msgs[1].Content), &payload); err != nil || payload["error"] == "" {
		t.Fatalf("tool result handed to the model does not report a failure: %q", msgs[1].Content)
	}

	select {
	case errMsg := <-audited:
		if errMsg == "" {
			t.Error("audit record for the failed call carries no reason")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the failed call was not audited as an error")
	}
}
