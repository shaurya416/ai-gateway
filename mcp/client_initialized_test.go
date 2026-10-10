package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ferro-labs/ai-gateway/pkg/logger"
)

// gatedMCPServer holds the client to the Streamable HTTP request contract the
// way the official Python SDK does before 1.18:
//
//   - a POST that does not offer both application/json and text/event-stream is
//     refused 406, notifications included;
//   - a request other than initialize is refused until notifications/initialized
//     has arrived, with the -32602 that SDK answers it with.
type gatedMCPServer struct {
	// refuseNotification, when non-zero, is the status the initialized
	// notification is answered with instead of 202.
	refuseNotification int

	mu          sync.Mutex
	initialized bool
}

func (s *gatedMCPServer) handle(w http.ResponseWriter, r *http.Request) {
	accept := strings.Join(r.Header.Values("Accept"), ",")
	if !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
		http.Error(w, "Not Acceptable: Client must accept both application/json and text/event-stream", http.StatusNotAcceptable)
		return
	}
	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if req.Method == "notifications/initialized" {
		if s.refuseNotification != 0 {
			http.Error(w, "notification refused", s.refuseNotification)
			return
		}
		s.mu.Lock()
		s.initialized = true
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := JSONRPCResponse{JSONRPC: "2.0", ID: req.ID}
	s.mu.Lock()
	initialized := s.initialized
	s.mu.Unlock()
	switch {
	case req.Method == mcpMethodInitialize:
		w.Header().Set("Mcp-Session-Id", "gated-session")
		resp.Result = initializeResult("gated", "1")
	case !initialized:
		resp.Error = &JSONRPCError{Code: -32602, Message: "Invalid request parameters"}
	case req.Method == mcpMethodToolsList:
		resp.Result = mustMarshal(map[string]any{"tools": []Tool{{Name: "gated_tool"}}})
	default:
		resp.Result = mustMarshal(ToolCallResult{Content: []ContentBlock{{Type: "text", Text: "ok"}}})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// The initialized notification must reach the server. It was posted without an
// Accept header, which a conformant server refuses, and the refusal was
// discarded — so a server that waits for the notification before serving
// requests answered every tools/list with an error, and never became ready.
func TestClientDeliversInitializedNotification(t *testing.T) {
	gated := &gatedMCPServer{}
	srv := httptest.NewServer(http.HandlerFunc(gated.handle))
	t.Cleanup(srv.Close)

	reg := NewRegistry(nil)
	t.Cleanup(func() { _ = reg.Close() })
	reg.RegisterConfig(ServerConfig{Name: "gated", URL: srv.URL, TimeoutSeconds: 5})
	reg.InitializeAll(t.Context(), func(name string, err error) {
		t.Errorf("server %s failed to initialize: %v", name, err)
	})

	gated.mu.Lock()
	initialized := gated.initialized
	gated.mu.Unlock()
	if !initialized {
		t.Fatal("the server never received notifications/initialized")
	}
	if !reg.IsReady("gated") || !reg.Owns("gated_tool") {
		t.Fatalf("server is not ready with its tool indexed: %+v", reg.Status())
	}
}

// A server that still refuses the notification is reported, not ignored. The
// handshake carries on — a server that does not gate on the notification
// remains usable — but the refusal is what explains every failure after it.
func TestClientReportsRefusedInitializedNotification(t *testing.T) {
	gated := &gatedMCPServer{refuseNotification: http.StatusBadRequest}
	srv := httptest.NewServer(http.HandlerFunc(gated.handle))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	old := logger.Default()
	logger.SetDefault(logger.New(logger.Options{Level: "info", Output: &buf}))
	t.Cleanup(func() { logger.SetDefault(old) })

	if _, err := NewClient(srv.URL, nil, 0).Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	logged := buf.String()
	for _, want := range []string{"notifications/initialized", "HTTP 400"} {
		if !strings.Contains(logged, want) {
			t.Errorf("refused notification not reported (missing %q): %s", want, logged)
		}
	}
}
