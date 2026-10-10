package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// replayedToolCallRequest is a conversation whose assistant turn carries a tool
// call with arguments that are not JSON — the partial object a model leaves
// when it is cut off mid-call, replayed verbatim by the client. OpenAI accepts
// any string there, so the same history reaches this provider on a failover.
func replayedToolCallRequest() core.Request {
	return core.Request{
		Model: "claude-sonnet-5",
		Messages: []core.Message{
			{Role: core.RoleUser, Content: "weather in Paris?"},
			{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{
				ID:       "toolu_1",
				Type:     "function",
				Function: core.FunctionCall{Name: "get_weather", Arguments: `{"city":"Par`},
			}}},
			{Role: core.RoleTool, ToolCallID: "toolu_1", Content: "sunny"},
		},
	}
}

// TestComplete_ToolCallWithMalformedArgumentsIsSent pins that a replayed tool
// call whose arguments are not JSON still reaches Anthropic, as an empty input
// object — the substitution the Bedrock-Anthropic and Gemini paths already make.
// Placed verbatim into the tool_use block's raw input, it failed the request
// body's encoding before any upstream call: a status-less error the gateway
// answers 500, retries, fails over and counts against the target's breaker.
func TestComplete_ToolCallWithMalformedArgumentsIsSent(t *testing.T) {
	body := captureBody(t, replayedToolCallRequest())

	msgs := decodeMessages(t, body)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	var blocks []struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(msgs[1]["content"], &blocks); err != nil {
		t.Fatalf("decode assistant blocks: %v", err)
	}
	if len(blocks) != 1 || blocks[0].Type != "tool_use" || blocks[0].ID != "toolu_1" {
		t.Fatalf("assistant blocks = %+v, want one tool_use toolu_1", blocks)
	}
	if string(blocks[0].Input) != "{}" {
		t.Errorf("tool_use input = %s, want {}", blocks[0].Input)
	}
}

// TestCompleteStream_ToolCallWithMalformedArgumentsIsSent is the streaming
// counterpart: both surfaces build the body the same way.
func TestCompleteStream_ToolCallWithMalformedArgumentsIsSent(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	p, err := New("test-key", srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch, err := p.CompleteStream(context.Background(), replayedToolCallRequest())
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	for range ch { //nolint:revive // drain the stream to completion
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}
