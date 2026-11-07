package middleware

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/pkg/logger"
)

const answerTestWriteTimeout = 200 * time.Millisecond

// serveWithWriteTimeout serves h from a server whose WriteTimeout is
// answerTestWriteTimeout and returns what a GET to it reads: the status, the
// body, and the error that ended the exchange, if any.
func serveWithWriteTimeout(t *testing.T, h http.Handler) (int, string, error) {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.WriteTimeout = answerTestWriteTimeout
	srv.Start()
	t.Cleanup(srv.Close)

	client := &http.Client{Transport: &http.Transport{}, Timeout: 10 * time.Second}
	t.Cleanup(client.Transport.(*http.Transport).CloseIdleConnections)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

// The answer gets the whole WriteTimeout from its first byte, however long it
// took to produce.
func TestWriteTimeoutFromAnswer_SlowAnswerIsDelivered(t *testing.T) {
	status, body, err := serveWithWriteTimeout(t, WriteTimeoutFromAnswer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * answerTestWriteTimeout)
		_, _ = io.WriteString(w, "answer")
	})))
	if err != nil || status != http.StatusOK || body != "answer" {
		t.Fatalf("got status %d body %q err %v, want 200 %q", status, body, err, "answer")
	}
}

// A handler that writes nothing still has its response delivered: net/http
// writes it once the handler returns.
func TestWriteTimeoutFromAnswer_EmptyAnswerIsDelivered(t *testing.T) {
	status, body, err := serveWithWriteTimeout(t, WriteTimeoutFromAnswer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * answerTestWriteTimeout)
		w.WriteHeader(http.StatusNoContent)
	})))
	if err != nil || status != http.StatusNoContent || body != "" {
		t.Fatalf("got status %d body %q err %v, want an empty 204", status, body, err)
	}
}

// A panic recovered further out is answered once the handler has unwound, and
// that answer gets the budget too.
func TestWriteTimeoutFromAnswer_RecoveredPanicIsAnswered(t *testing.T) {
	h := RecoverJSON(logger.Default())(WriteTimeoutFromAnswer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(2 * answerTestWriteTimeout)
		panic("late failure")
	})))
	status, _, err := serveWithWriteTimeout(t, h)
	if err != nil || status != http.StatusInternalServerError {
		t.Fatalf("got status %d err %v, want the 500 envelope", status, err)
	}
}

// The bound still holds once the answer has started: an answer that stalls
// past WriteTimeout after its first byte is cut off, so a caller that stops
// reading cannot hold the connection.
func TestWriteTimeoutFromAnswer_StalledAnswerIsStillBounded(t *testing.T) {
	_, body, err := serveWithWriteTimeout(t, WriteTimeoutFromAnswer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "first")
		_ = http.NewResponseController(w).Flush()
		time.Sleep(2 * answerTestWriteTimeout)
		_, _ = io.WriteString(w, "second")
		_ = http.NewResponseController(w).Flush()
	})))
	if err == nil && body == "firstsecond" {
		t.Fatal("an answer that stalled past WriteTimeout after its first byte was delivered whole")
	}
}

// A hijacked connection belongs to whoever took it: nothing re-arms its write
// deadline when the handler returns.
func TestWriteTimeoutFromAnswer_HijackedConnectionIsNotTouched(t *testing.T) {
	srv := httptest.NewUnstartedServer(WriteTimeoutFromAnswer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			time.Sleep(2 * answerTestWriteTimeout)
			_, _ = brw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 6\r\nConnection: close\r\n\r\ntunnel")
			_ = brw.Flush()
		}()
	})))
	srv.Config.WriteTimeout = answerTestWriteTimeout
	srv.Start()
	t.Cleanup(srv.Close)

	client := &http.Client{Transport: &http.Transport{}, Timeout: 10 * time.Second}
	t.Cleanup(client.Transport.(*http.Transport).CloseIdleConnections)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("the hijacker's late write failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if body, err := io.ReadAll(resp.Body); err != nil || string(body) != "tunnel" {
		t.Fatalf("body %q err %v, want %q", body, err, "tunnel")
	}
}

// deadlineLog is a writer that records every write deadline set on it.
type deadlineLog struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	hijacked  bool
}

func (d *deadlineLog) SetWriteDeadline(deadline time.Time) error {
	d.deadlines = append(d.deadlines, deadline)
	return nil
}

func (d *deadlineLog) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	d.hijacked = true
	return nil, nil, nil
}

// serveLogged runs h behind WriteTimeoutFromAnswer for a request served by a
// server with a one-minute WriteTimeout, and returns the deadlines set.
func serveLogged(t *testing.T, h http.HandlerFunc) []time.Time {
	t.Helper()
	w := &deadlineLog{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), http.ServerContextKey, &http.Server{WriteTimeout: time.Minute, ReadHeaderTimeout: time.Second}))
	WriteTimeoutFromAnswer(h).ServeHTTP(w, r)
	return w.deadlines
}

// The deadline is armed once, from the answer's first byte rather than from
// the moment the handler was entered.
func TestWriteTimeoutFromAnswer_ArmsOnceFromTheFirstByte(t *testing.T) {
	var firstByte time.Time
	deadlines := serveLogged(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(20 * time.Millisecond)
		firstByte = time.Now()
		_, _ = io.WriteString(w, "a")
		_ = http.NewResponseController(w).Flush()
		_, _ = io.WriteString(w, "b")
	})
	if len(deadlines) != 1 {
		t.Fatalf("write deadline set %d times, want once", len(deadlines))
	}
	if d := deadlines[0]; d.Before(firstByte.Add(time.Minute)) {
		t.Fatalf("deadline %v is earlier than WriteTimeout from the first byte (%v)", d, firstByte.Add(time.Minute))
	}
}

// A write deadline the handler sets itself is the one that stands: the
// streaming paths bound each write their own way, and the first byte must not
// replace that bound with WriteTimeout.
func TestWriteTimeoutFromAnswer_HandlerDeadlineStands(t *testing.T) {
	own := time.Now().Add(15 * time.Second)
	deadlines := serveLogged(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(own)
		_, _ = io.WriteString(w, "chunk")
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	})
	if len(deadlines) != 2 || !deadlines[0].Equal(own) || !deadlines[1].IsZero() {
		t.Fatalf("deadlines = %v, want only the handler's own [%v, zero]", deadlines, own)
	}
}

// A hijacked connection's deadline is the hijacker's.
func TestWriteTimeoutFromAnswer_HijackSetsNoDeadline(t *testing.T) {
	deadlines := serveLogged(t, func(w http.ResponseWriter, _ *http.Request) {
		if _, _, err := http.NewResponseController(w).Hijack(); err != nil {
			t.Fatalf("hijack: %v", err)
		}
	})
	if len(deadlines) != 0 {
		t.Fatalf("deadlines = %v after a hijack, want none", deadlines)
	}
}

// With no WriteTimeout to re-arm, the handler is given the writer untouched.
func TestWriteTimeoutFromAnswer_NoWriteTimeoutLeavesTheWriterAlone(t *testing.T) {
	for name, srv := range map[string]*http.Server{"no server": nil, "no WriteTimeout": {}} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			if srv != nil {
				r = r.WithContext(context.WithValue(r.Context(), http.ServerContextKey, srv))
			}
			WriteTimeoutFromAnswer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if w != rec {
					t.Errorf("handler got %T, want the server's own writer", w)
				}
			})).ServeHTTP(rec, r)
		})
	}
}
