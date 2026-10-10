package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// A server may ping its client at any time, and the spec obliges the receiver
// to answer with an empty result. The stdio transport started without a
// request handler, so it refused every request a server sent, ping included.
// The official Go SDK before v1.6 closes the session when a keepalive ping is
// refused: a stdio server built on it with KeepAlive set exited one interval
// after its handshake, and its tools were withdrawn.
func TestStdioClientAnswersServerPing(t *testing.T) {
	client := startHelperClient(t, "pinger", helperModeServe)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, err := client.CallTool(ctx, helperToolPingClient, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	got, err := toolResultContent(result)
	if err != nil {
		t.Fatalf("toolResultContent: %v", err)
	}
	if got != helperPingAnswered {
		t.Fatalf("server's ping to the client = %q, want %q", got, helperPingAnswered)
	}
}
