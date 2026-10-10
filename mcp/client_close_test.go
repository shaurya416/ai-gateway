package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// sessionEndingServer issues a session on initialize and records every DELETE
// it is sent, answering each with deleteStatus.
type sessionEndingServer struct {
	*httptest.Server
	deleteStatus int

	mu      sync.Mutex
	deletes []http.Header
}

func newSessionEndingServer(t *testing.T, deleteStatus int) *sessionEndingServer {
	t.Helper()
	s := &sessionEndingServer{deleteStatus: deleteStatus}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

func (s *sessionEndingServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		s.mu.Lock()
		s.deletes = append(s.deletes, r.Header.Clone())
		s.mu.Unlock()
		w.WriteHeader(s.deleteStatus)
		return
	}
	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.ID == nil { // notifications/initialized
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Mcp-Session-Id", "session-1")
	_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: initializeResult("ending", "1")})
}

func (s *sessionEndingServer) deleteHeaders() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]http.Header(nil), s.deletes...)
}

func initializedClient(t *testing.T, url string) *Client {
	t.Helper()
	c := NewClient(url, map[string]string{"Authorization": "Bearer operator-token"}, 5*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return c
}

// A client that no longer needs its session should end it with a DELETE
// carrying the session ID. Close sent nothing, so every retired client — one per
// HTTP server on each configuration reload and each restart — left its session
// open on the server.
func TestClientCloseEndsTheSession(t *testing.T) {
	srv := newSessionEndingServer(t, http.StatusOK)
	c := initializedClient(t, srv.URL)

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	deletes := srv.deleteHeaders()
	if len(deletes) != 1 {
		t.Fatalf("server received %d DELETE requests, want exactly 1", len(deletes))
	}
	if got := deletes[0].Get("Mcp-Session-Id"); got != "session-1" {
		t.Errorf("DELETE carried session %q, want session-1", got)
	}
	// The server authenticates the DELETE like any other request.
	if got := deletes[0].Get("Authorization"); got != "Bearer operator-token" {
		t.Errorf("DELETE carried Authorization %q, want the configured header", got)
	}
}

// A server may refuse to let clients end sessions (405), and one that already
// ended the session answers 404. Neither leaves anything for the client to do.
func TestClientCloseAcceptsRefusalAndExpiredSession(t *testing.T) {
	for _, status := range []int{http.StatusMethodNotAllowed, http.StatusNotFound} {
		srv := newSessionEndingServer(t, status)
		c := initializedClient(t, srv.URL)
		if err := c.Close(); err != nil {
			t.Errorf("Close against a DELETE answered %d: %v", status, err)
		}
		if n := len(srv.deleteHeaders()); n != 1 {
			t.Errorf("server answering %d received %d DELETE requests, want 1", status, n)
		}
	}
}

// A refusal the spec does not describe is reported rather than read as the
// session having ended.
func TestClientCloseReportsAFailedDelete(t *testing.T) {
	srv := newSessionEndingServer(t, http.StatusInternalServerError)
	c := initializedClient(t, srv.URL)
	if err := c.Close(); err == nil {
		t.Fatal("Close returned nil for a DELETE the server answered 500")
	}
}

// Without a session there is nothing to end, and nothing is sent.
func TestClientCloseWithoutSessionSendsNothing(t *testing.T) {
	srv := newSessionEndingServer(t, http.StatusOK)
	c := NewClient(srv.URL, nil, time.Second)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(srv.deleteHeaders()); n != 0 {
		t.Fatalf("server received %d DELETE requests from a client with no session", n)
	}
}
