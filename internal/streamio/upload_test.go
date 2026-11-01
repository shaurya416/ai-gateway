package streamio

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// readDeadlineRecorder records every read deadline set through it.
type readDeadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (r *readDeadlineRecorder) SetReadDeadline(deadline time.Time) error {
	r.deadlines = append(r.deadlines, deadline)
	return nil
}

func uploadRequest(t *testing.T, body string, srv *http.Server) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/files", strings.NewReader(body))
	if srv != nil {
		r = r.WithContext(context.WithValue(r.Context(), http.ServerContextKey, srv))
	}
	return r
}

func TestBoundUploadByIdleness_PushesTheReadDeadlineBeforeEachRead(t *testing.T) {
	const readTimeout = time.Minute
	w := &readDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	r := uploadRequest(t, "abcdef", &http.Server{ReadTimeout: readTimeout})

	release := BoundUploadByIdleness(w, r)
	defer release()

	before := time.Now()
	buf := make([]byte, 2)
	for range 3 {
		if _, err := r.Body.Read(buf); err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if len(w.deadlines) != 3 {
		t.Fatalf("read deadline set %d times over 3 reads, want once per read", len(w.deadlines))
	}
	for i, d := range w.deadlines {
		if d.Before(before.Add(readTimeout)) || d.After(time.Now().Add(readTimeout)) {
			t.Errorf("deadline %d = %v, want ReadTimeout from the read that set it", i, d)
		}
	}
}

// After release the body may still be read — the reverse proxy's transport can
// outlive the handler — and a deadline set then would land on the connection's
// next request. After the body's last read, net/http has re-armed the connection
// for its own background read, which a deadline set then would cut short.
func TestBoundUploadByIdleness_LeavesTheDeadlineAloneAfterReleaseOrEnd(t *testing.T) {
	t.Run("after release", func(t *testing.T) {
		w := &readDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		r := uploadRequest(t, "abcdef", &http.Server{ReadTimeout: time.Minute})
		release := BoundUploadByIdleness(w, r)
		release()
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(w.deadlines) != 0 {
			t.Fatalf("read deadline set %d times after release, want none", len(w.deadlines))
		}
	})

	t.Run("after end of body", func(t *testing.T) {
		w := &readDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		r := uploadRequest(t, "abc", &http.Server{ReadTimeout: time.Minute})
		release := BoundUploadByIdleness(w, r)
		defer release()
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Fatalf("read: %v", err)
		}
		set := len(w.deadlines)
		for range 3 {
			if _, err := r.Body.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("read past end = %v, want io.EOF", err)
			}
		}
		if len(w.deadlines) != set {
			t.Fatalf("read deadline set %d more times after end of body, want none", len(w.deadlines)-set)
		}
	})
}

// With no ReadTimeout there is no bound to relax, and outside an http.Server
// there is no deadline to move; the body is left exactly as it was.
func TestBoundUploadByIdleness_NoServerReadTimeoutChangesNothing(t *testing.T) {
	for name, srv := range map[string]*http.Server{
		"no server":      nil,
		"no ReadTimeout": {},
	} {
		t.Run(name, func(t *testing.T) {
			w := &readDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			r := uploadRequest(t, "abc", srv)
			body := r.Body
			release := BoundUploadByIdleness(w, r)
			defer release()
			if r.Body != body {
				t.Fatal("body was wrapped with no ReadTimeout to relax")
			}
		})
	}
}

func TestRestartWriteTimeout_GivesTheAnswerTheWholeWriteTimeoutFromNow(t *testing.T) {
	const writeTimeout = time.Minute
	w := newDeadlineRecorder()
	r := uploadRequest(t, "", &http.Server{WriteTimeout: writeTimeout})

	before := time.Now()
	RestartWriteTimeout(w, r)

	if len(w.deadlines) != 1 {
		t.Fatalf("write deadline set %d times, want once", len(w.deadlines))
	}
	if d := w.deadlines[0]; d.Before(before.Add(writeTimeout)) || d.After(time.Now().Add(writeTimeout)) {
		t.Errorf("deadline = %v, want WriteTimeout from the call", d)
	}

	for name, srv := range map[string]*http.Server{"no server": nil, "no WriteTimeout": {}} {
		t.Run(name, func(t *testing.T) {
			w := newDeadlineRecorder()
			RestartWriteTimeout(w, uploadRequest(t, "", srv))
			if len(w.deadlines) != 0 {
				t.Fatalf("write deadline set %d times with no WriteTimeout to restart, want none", len(w.deadlines))
			}
		})
	}
}
