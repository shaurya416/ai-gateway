package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Tool arguments must reach a stdio server as the model wrote them. They were
// decoded into an untyped value on the way, which holds every number as a
// float64, so an integer past 2^53 — a snowflake or a bigint row id — reached
// the server rounded to a different value, while the HTTP transport forwards
// the same arguments untouched.
func TestStdioClientForwardsLargeIntegerArgumentsExactly(t *testing.T) {
	client := startHelperClient(t, "args", helperModeServe)

	const args = `{"user_id":1234567890123456789,"limit":10}`
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, err := client.CallTool(ctx, helperToolArgs, json.RawMessage(args))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	got, err := toolResultContent(result)
	if err != nil {
		t.Fatalf("toolResultContent: %v", err)
	}
	if got != args {
		t.Fatalf("arguments the server received = %s, want %s", got, args)
	}
}
