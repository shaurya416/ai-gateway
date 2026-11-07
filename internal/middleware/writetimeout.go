package middleware

import (
	"bufio"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// WriteTimeoutFromAnswer re-arms the serving http.Server's WriteTimeout when a
// handler starts to write its answer, so the timeout bounds writing the answer
// rather than producing it.
//
// net/http arms WriteTimeout once it has read a request's headers. Every route
// that waits on a provider spends that wait inside the handler, before the
// first byte, so an answer that took longer than WriteTimeout to produce was
// served, logged and billed while the caller read a closed connection. The
// work is bounded by request_timeout; WriteTimeout is left to bound a caller
// that stops reading.
//
// This is the one place the gateway moves the write deadline on a handler's
// behalf. It is re-armed once: on the answer's first Write or flush, or when
// the handler returns having written nothing, since net/http writes the
// response then — and a panic recovered further out is answered then too. A
// status line alone arms nothing: it is buffered until one of those, and an
// informational 1xx relayed ahead of the answer is not the answer.
//
// A handler that sets a write deadline itself owns it from that call on, and
// the first byte does not re-arm over it: the streaming paths set one around
// every write as they make it. No handler may set one before its answer is
// ready, because that deadline would then bound the work instead. A hijacked
// connection is never touched, and a server with no WriteTimeout is left
// alone.
func WriteTimeoutFromAnswer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv, _ := r.Context().Value(http.ServerContextKey).(*http.Server)
		if srv == nil || srv.WriteTimeout <= 0 {
			next.ServeHTTP(w, r)
			return
		}
		aw := &answerWriter{
			ResponseWriter: w,
			controller:     http.NewResponseController(w),
			budget:         srv.WriteTimeout,
		}
		// Deferred, so it also runs while a panic unwinds to RecoverJSON.
		defer aw.answer()
		next.ServeHTTP(aw, r)
	})
}

// answerWriter is the writer WriteTimeoutFromAnswer installs. It implements
// Unwrap so http.ResponseController still reaches the real writer's read
// deadline and full-duplex switch.
type answerWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
	budget     time.Duration
	// answered is set once the write deadline belongs to the answer: re-armed
	// here, set by the handler, or handed over with the connection.
	answered atomic.Bool
}

func (a *answerWriter) Unwrap() http.ResponseWriter { return a.ResponseWriter }

// answer gives the answer the whole budget from now, the first time only.
func (a *answerWriter) answer() {
	if a.answered.CompareAndSwap(false, true) {
		_ = a.controller.SetWriteDeadline(time.Now().Add(a.budget))
	}
}

func (a *answerWriter) Write(p []byte) (int, error) {
	a.answer()
	return a.ResponseWriter.Write(p)
}

// FlushError is the method http.ResponseController prefers for a flush.
func (a *answerWriter) FlushError() error {
	a.answer()
	return a.controller.Flush()
}

// SetWriteDeadline is the handler taking the write deadline over.
func (a *answerWriter) SetWriteDeadline(deadline time.Time) error {
	a.answered.Store(true)
	return a.controller.SetWriteDeadline(deadline)
}

// Hijack hands the connection, and with it the write deadline, to the caller.
func (a *answerWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := a.controller.Hijack()
	if err == nil {
		a.answered.Store(true)
	}
	return conn, brw, err
}
