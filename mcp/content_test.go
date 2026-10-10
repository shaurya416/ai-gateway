package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// registryAnswering builds a ready registry over an HTTP server that exposes
// toolName and answers every call to it with result.
func registryAnswering(t *testing.T, toolName string, result any) *Registry {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var res json.RawMessage
		switch req.Method {
		case mcpMethodInitialize:
			res = initializeResult("content", "1")
		case mcpMethodToolsList:
			res = mustMarshal(map[string]any{"tools": []Tool{{Name: toolName}}})
		case mcpMethodToolsCall:
			res = mustMarshal(result)
		default:
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: res})
	}))
	t.Cleanup(srv.Close)

	reg := NewRegistry(nil)
	t.Cleanup(func() { _ = reg.Close() })
	reg.RegisterConfig(ServerConfig{Name: "content-srv", URL: srv.URL, TimeoutSeconds: 5})
	reg.InitializeAll(t.Context(), func(name string, err error) {
		t.Fatalf("init %s: %v", name, err)
	})
	return reg
}

// assertCarriesResourceLink fails unless content still names the resource the
// helper's resource_link block points at.
func assertCarriesResourceLink(t *testing.T, content string) {
	t.Helper()
	for _, field := range []string{"uri", "name", "title", "description"} {
		want, _ := helperResourceLink[field].(string)
		if !strings.Contains(content, want) {
			t.Errorf("tool result handed to the model lost the link's %s %q: %s", field, want, content)
		}
	}
}

// A resource_link block is a tool's answer in its own right: the uri is the
// result. Decoding it into a block that models only text, image and embedded
// resources kept the type and dropped the uri, name and description, so the
// model was told the tool had returned a link to nothing.
func TestToolResultKeepsResourceLinkOverHTTP(t *testing.T) {
	reg := registryAnswering(t, "find_doc", map[string]any{
		"content": []map[string]any{helperResourceLink},
	})

	msgs, err := NewExecutor(reg, 5, nil).ResolvePendingToolCalls(context.Background(), toolCallResponse("find_doc"))
	if err != nil {
		t.Fatalf("ResolvePendingToolCalls: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want the assistant turn and one tool result", len(msgs))
	}
	assertCarriesResourceLink(t, msgs[1].Content)
}

// A link's size is optional metadata the model reads. A server that writes it
// as a float literal — what Python's json module emits for a float — must not
// fail the whole tool call over it.
func TestToolResultResourceLinkKeepsSizeAsWritten(t *testing.T) {
	reg := registryAnswering(t, "find_doc", json.RawMessage(
		`{"content":[{"type":"resource_link","uri":"file:///workspace/a.bin","name":"a.bin","size":1024.0}]}`,
	))

	msgs, err := NewExecutor(reg, 5, nil).ResolvePendingToolCalls(context.Background(), toolCallResponse("find_doc"))
	if err != nil {
		t.Fatalf("ResolvePendingToolCalls: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want the assistant turn and one tool result", len(msgs))
	}
	for _, want := range []string{"file:///workspace/a.bin", `"size":1024.0`} {
		if !strings.Contains(msgs[1].Content, want) {
			t.Errorf("tool result handed to the model lacks %s: %s", want, msgs[1].Content)
		}
	}
}

// The stdio transport converts the library's typed result through the same
// block, so it lost the same fields.
func TestToolResultKeepsResourceLinkOverStdio(t *testing.T) {
	client := startHelperClient(t, "links", helperModeServe)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, err := client.CallTool(ctx, helperToolLink, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	content, err := contentBlocksToString(result.Content)
	if err != nil {
		t.Fatalf("contentBlocksToString: %v", err)
	}
	assertCarriesResourceLink(t, content)
}
