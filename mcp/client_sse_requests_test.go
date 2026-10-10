package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// A server may send requests of its own on the stream that answers a POST, ahead
// of the answer, and the official Go SDK sends any a tool makes with its
// request's context there — a ping, or a roots/list. A server that waits for the
// reply before it answers cannot answer while the client waits for the stream to
// end, so the client must reply as the requests arrive. It read the whole body
// first, so the tool stalled until its own wait ran out — or, with none, until
// the gateway's HTTP timeout failed the call — while the same tool served over
// stdio was answered at once.
func TestClientAnswersRequestsOnAPostStream(t *testing.T) {
	const sessionID = "session-post-stream"
	type reply struct {
		sessionID string
		result    json.RawMessage
		err       *JSONRPCError
	}
	var (
		mu      sync.Mutex
		posted  = map[string]reply{}
		arrived = make(chan struct{}, 2)
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "no event stream", http.StatusMethodNotAllowed)
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *JSONRPCError   `json:"error"`
		}
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		switch msg.Method {
		case mcpMethodInitialize:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", sessionID)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": msg.ID, "result": initializeResult("post-stream", "1"),
			})
		case mcpMethodToolsCall:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"srv-ping\",\"method\":\"ping\"}\n\n")
			_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"srv-roots\",\"method\":\"roots/list\"}\n\n")
			w.(http.Flusher).Flush()

			// Both requests are waited on before the answer, as a tool that
			// needs their replies to finish would.
			answer := "requests answered"
			for range 2 {
				select {
				case <-arrived:
				case <-time.After(2 * time.Second):
					answer = "requests unanswered"
				case <-r.Context().Done():
					return
				}
			}
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", mustMarshal(JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Result:  mustMarshal(ToolCallResult{Content: []ContentBlock{{Type: "text", Text: answer}}}),
			}))
		case "":
			// The client's reply to one of the requests above.
			var id string
			_ = json.Unmarshal(msg.ID, &id)
			mu.Lock()
			posted[id] = reply{sessionID: r.Header.Get("Mcp-Session-Id"), result: msg.Result, err: msg.Error}
			mu.Unlock()
			arrived <- struct{}{}
			w.WriteHeader(http.StatusAccepted)
		default: // notifications/initialized
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, nil, 10*time.Second)
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	result, err := c.CallTool(context.Background(), "uses_session", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "requests answered" {
		t.Fatalf("CallTool = %+v; want the tool to have had its requests answered", result)
	}

	mu.Lock()
	defer mu.Unlock()
	ping := posted["srv-ping"]
	if ping.sessionID != sessionID {
		t.Errorf("ping answer carried session %q, want %q", ping.sessionID, sessionID)
	}
	if ping.err != nil || string(ping.result) != "{}" {
		t.Errorf("ping answer = result %s, error %+v; want the empty result the spec requires", ping.result, ping.err)
	}
	// The gateway declares no client capabilities, so anything else is refused
	// at once rather than left for the server to wait on.
	if roots := posted["srv-roots"]; roots.err == nil || roots.err.Code != -32601 {
		t.Errorf("roots/list answer = result %s, error %+v; want method not found (-32601)", roots.result, roots.err)
	}
}
