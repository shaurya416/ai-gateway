package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pingingServer issues a session, and on the event stream the client opens for
// it sends one ping and one request the gateway does not serve. It records what
// the client posts back.
type pingingServer struct {
	*httptest.Server

	mu      sync.Mutex
	replies map[string]postedReply

	streamOpened chan struct{}
	streamEnded  chan struct{}
	openOnce     sync.Once

	// breakFirstStream cuts the connection under the first stream the client
	// opens, as a proxy that drops it does, and serves the requests on the next.
	breakFirstStream bool
	streams          atomic.Int32
	// refuseFirstStream, when set, answers the first stream the client opens
	// with that status, and serves the requests on the next.
	refuseFirstStream atomic.Int32
}

// postedReply is one JSON-RPC response the client posted, with the session it
// named.
type postedReply struct {
	SessionID string
	Result    json.RawMessage
	Error     *JSONRPCError
}

func newPingingServer(t *testing.T, breakFirstStream bool) *pingingServer {
	t.Helper()
	s := &pingingServer{
		replies:          map[string]postedReply{},
		streamOpened:     make(chan struct{}),
		streamEnded:      make(chan struct{}),
		breakFirstStream: breakFirstStream,
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

func (s *pingingServer) handle(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.serveStream(w, r)
		return
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
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
	switch {
	case msg.Method == mcpMethodInitialize:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "session-events")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": msg.ID, "result": initializeResult("pinging", "1"),
		})
	case msg.Method != "": // notifications/initialized
		w.WriteHeader(http.StatusAccepted)
	default: // the client's answer to a request the server sent
		var id string
		_ = json.Unmarshal(msg.ID, &id)
		s.mu.Lock()
		s.replies[id] = postedReply{SessionID: r.Header.Get("Mcp-Session-Id"), Result: msg.Result, Error: msg.Error}
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}
}

// serveStream sends the server's requests, then holds the stream open until the
// client goes away, as a server's standalone stream does.
func (s *pingingServer) serveStream(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Mcp-Session-Id") != "session-events" {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	first := s.streams.Add(1) == 1
	if status := int(s.refuseFirstStream.Load()); first && status != 0 {
		http.Error(w, "stream unavailable", status)
		return
	}
	if first && s.breakFirstStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	defer close(s.streamEnded)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "id: 1\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"srv-ping\",\"method\":\"ping\"}\n\n")
	_, _ = fmt.Fprint(w, "id: 2\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"srv-roots\",\"method\":\"roots/list\"}\n\n")
	w.(http.Flusher).Flush()
	s.openOnce.Do(func() { close(s.streamOpened) })
	<-r.Context().Done()
}

// reply waits for the client's answer to the request with id.
func (s *pingingServer) reply(t *testing.T, id string) postedReply {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		got, ok := s.replies[id]
		s.mu.Unlock()
		if ok {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the client never answered the server's %q request", id)
	return postedReply{}
}

// A server reaches its client outside an answer to a request only over the
// event stream the client opens after the handshake, and pings it there. The
// client never opened one, so a ping had nowhere to go: the official Go SDK
// with KeepAlive set counts an undelivered ping as a failed one and closes the
// session, cutting off whatever tool call was in flight at that moment, once
// per keepalive interval.
func TestClientAnswersPingsOnTheEventStream(t *testing.T) {
	srv := newPingingServer(t, false)
	c := initializedClient(t, srv.URL)

	ping := srv.reply(t, "srv-ping")
	if ping.SessionID != "session-events" {
		t.Errorf("ping answer carried session %q, want session-events", ping.SessionID)
	}
	if ping.Error != nil || string(ping.Result) != "{}" {
		t.Errorf("ping answer = result %s, error %+v; want the empty result the spec requires", ping.Result, ping.Error)
	}

	// The gateway declares no client capabilities, so any other request is
	// refused rather than left to time out on the server.
	roots := srv.reply(t, "srv-roots")
	if roots.Error == nil || roots.Error.Code != -32601 {
		t.Errorf("roots/list answer = result %s, error %+v; want method not found (-32601)", roots.Result, roots.Error)
	}

	// Close ends the stream before it returns, so nothing is left reading it.
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-srv.streamEnded:
	case <-time.After(5 * time.Second):
		t.Fatal("the event stream was still open after Close returned")
	}
}

// A stream that breaks — a proxy or load balancer dropping the connection — is
// reopened like one the server ended; only a refusal is final. Giving up on it
// left the server's pings nowhere to go for the rest of the session.
func TestClientReopensABrokenEventStream(t *testing.T) {
	srv := newPingingServer(t, true)
	c := initializedClient(t, srv.URL)
	t.Cleanup(func() { _ = c.Close() })

	ping := srv.reply(t, "srv-ping")
	if ping.Error != nil || string(ping.Result) != "{}" {
		t.Errorf("ping answer = result %s, error %+v; want the empty result the spec requires", ping.Result, ping.Error)
	}
	if got := srv.streams.Load(); got != 2 {
		t.Errorf("client opened the event stream %d times, want 2: once, and again after it broke", got)
	}
}

// A server that offers no event stream answers 405, and the client must take
// that as final rather than asking again.
func TestClientAcceptsAServerWithoutAnEventStream(t *testing.T) {
	var (
		mu   sync.Mutex
		gets int
	)
	ending := &sessionEndingServer{deleteStatus: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			mu.Lock()
			gets++
			mu.Unlock()
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ending.handle(w, r)
	}))
	t.Cleanup(srv.Close)

	c := initializedClient(t, srv.URL)
	// Longer than the shortest wait before a stream is reopened.
	time.Sleep(listenRetryDelay + listenRetryDelay/2)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gets != 1 {
		t.Fatalf("client asked for the event stream %d times, want once: 405 means the server offers none", gets)
	}
}

// A status that says "not now" — the official Go SDK's 409 while it still holds
// the stream a dropped connection left behind, or a 503 from a proxy whose
// backend is restarting — is retried like a stream that broke. Taking it as
// final left the session with no stream for the rest of its life, so the next
// keepalive ping went undelivered and the server closed the session.
func TestClientReopensAnEventStreamRefusedForNow(t *testing.T) {
	for _, status := range []int32{http.StatusConflict, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(int(status)), func(t *testing.T) {
			srv := newPingingServer(t, false)
			srv.refuseFirstStream.Store(status)
			c := initializedClient(t, srv.URL)
			t.Cleanup(func() { _ = c.Close() })

			ping := srv.reply(t, "srv-ping")
			if ping.Error != nil || string(ping.Result) != "{}" {
				t.Errorf("ping answer = result %s, error %+v; want the empty result the spec requires", ping.Result, ping.Error)
			}
		})
	}
}

// An event stream carrying more than the size bound is refused, not reopened:
// the server would only send it again.
func TestClientRefusesAnOversizedEventStream(t *testing.T) {
	oversized := map[string]string{
		"line":  "data: " + strings.Repeat("x", maxResponseBodyBytes) + "\n\n",
		"event": strings.Repeat("data: "+strings.Repeat("x", maxResponseBodyBytes/2)+"\n", 3) + "\n",
	}
	for name, body := range oversized {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, body)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(srv.URL, nil, 5*time.Second)
			if _, _, err := c.readEvents(t.Context(), "session"); !errors.Is(err, errEventStreamRefused) {
				t.Fatalf("readEvents error = %v, want it refused", err)
			}
		})
	}
}

// The bound covers an event's data as it is dispatched, joining newlines
// included. Only the text after each data: prefix was counted, so an event of
// empty data: lines — which adds nothing to that count — accumulated one entry
// per line for as long as the server kept sending them, and a server streaming
// them without a blank line grew gateway memory without limit.
func TestClientRefusesAnEventOfEmptyDataLines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := []byte(strings.Repeat("data:\n", 64<<10))
		// More lines than the bound has bytes, in whole chunks, and then the
		// blank line that would dispatch the event.
		for sent := 0; sent <= maxResponseBodyBytes; sent += 64 << 10 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		_, _ = fmt.Fprint(w, "\n")
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, nil, 30*time.Second)
	if _, _, err := c.readEvents(t.Context(), "session"); !errors.Is(err, errEventStreamRefused) {
		t.Fatalf("readEvents error = %v, want the event refused once its data passed the bound", err)
	}
}

// A retry interval the server names is honoured only down to the client's own
// shortest wait. Taken as given, a server that asks for one millisecond and
// then ends every stream at once had the client reopen it a thousand times a
// second for the life of the session.
func TestClientBoundsTheServersRetryInterval(t *testing.T) {
	var gets atomic.Int32
	ending := &sessionEndingServer{deleteStatus: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "retry: 1\n\n")
			return
		}
		ending.handle(w, r)
	}))
	t.Cleanup(srv.Close)

	c := initializedClient(t, srv.URL)
	// Well inside the shortest wait, and long enough for an unbounded client
	// to reopen the stream hundreds of times.
	time.Sleep(listenRetryDelay / 2)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := gets.Load(); got != 1 {
		t.Fatalf("client opened the event stream %d times in half its shortest wait, want once", got)
	}
}

// reissuingStreamServer issues session-1 on every initialize, as a server that
// numbers sessions in memory does after a restart, and serves an event stream
// for a session it currently holds. A GET for one it does not hold is 404.
type reissuingStreamServer struct {
	*httptest.Server

	mu        sync.Mutex
	known     bool
	restarted chan struct{}

	opened  chan struct{} // one send per stream served
	refused chan struct{} // one send per GET answered 404
}

func newReissuingStreamServer(t *testing.T) *reissuingStreamServer {
	t.Helper()
	s := &reissuingStreamServer{
		restarted: make(chan struct{}),
		opened:    make(chan struct{}, 8),
		refused:   make(chan struct{}, 8),
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

// restart drops the session and ends the stream serving it.
func (s *reissuingStreamServer) restart() {
	s.mu.Lock()
	s.known = false
	close(s.restarted)
	s.restarted = make(chan struct{})
	s.mu.Unlock()
}

func (s *reissuingStreamServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	known := s.known && r.Header.Get("Mcp-Session-Id") == "session-1"
	restarted := s.restarted
	s.mu.Unlock()

	if r.Method == http.MethodGet {
		if !known {
			s.refused <- struct{}{}
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// The shortest interval the client honours, so the reopen after the
		// restart below comes a second later rather than after the backoff.
		_, _ = fmt.Fprint(w, "retry: 1\n\n")
		w.(http.Flusher).Flush()
		s.opened <- struct{}{}
		select {
		case <-restarted:
		case <-r.Context().Done():
		}
		return
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusOK)
		return
	}

	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	switch req.Method {
	case mcpMethodInitialize:
		s.mu.Lock()
		s.known = true
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "session-1")
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: initializeResult("reissuing", "1")})
	case mcpMethodToolsCall:
		if !known {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{
			JSONRPC: "2.0", ID: req.ID,
			Result: mustMarshal(ToolCallResult{Content: []ContentBlock{{Type: "text", Text: "ok"}}}),
		})
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

// A renewal that is handed back the session ID the client already held still
// opens an event stream for it. The stream of the expired session can already
// be over — a reopen after the restart met 404, which ends it for good — so
// keying the new stream on a changed ID left the renewed session with none,
// and the server's keepalive pings nowhere to go, for the rest of the process.
func TestClientReopensTheEventStreamForAReissuedSession(t *testing.T) {
	srv := newReissuingStreamServer(t)
	c := initializedClient(t, srv.URL)
	t.Cleanup(func() { _ = c.Close() })

	waitFor := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}
	waitFor(srv.opened, "the first event stream")

	srv.restart()
	waitFor(srv.refused, "the reopened stream to meet the restarted server's 404")

	if _, err := c.CallTool(t.Context(), "t", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool after the restart: %v", err)
	}
	if got := c.getSessionID(); got != "session-1" {
		t.Fatalf("session ID = %q, want the reissued session-1", got)
	}
	waitFor(srv.opened, "an event stream for the renewed session")
}
