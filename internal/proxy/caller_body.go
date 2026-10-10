package proxy

import (
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/ferro-labs/ai-gateway/internal/apierror"
)

// callerBody is the request body a forward streams upstream. It remembers the
// first failure reading it, so the forward can tell a body the caller could not
// deliver from an upstream it could not reach.
//
// The transport reports both through the reverse proxy's ErrorHandler as one
// error, and read as the second, a body whose chunked framing broke mid-upload
// was answered 502 "upstream connection failed" and scored against the
// target's circuit breaker: a handful of such requests from one caller opened
// the circuit for every caller.
//
// onFail, when non-nil, runs once, on the first failure. A governed forward
// passes the cancel of a context its lifecycle runs under, which ends the
// request as the caller's. That is what net/http already does when the
// caller's connection breaks mid-body, and it keeps the failure off the
// breaker; a body that breaks over a live connection, or runs past the
// gateway's own size limit, leaves the connection and its context untouched.
//
// The transport reads the body from its own goroutine, which can outlive the
// forward, so the state is guarded. A failure after Close is not counted: the
// reverse proxy closes the body once it is done with the request, whatever the
// transport is still doing.
type callerBody struct {
	io.ReadCloser
	onFail func()

	mu     sync.Mutex
	closed bool
	err    error
}

// watchCallerBody installs a callerBody as r's body and returns it. A request
// with no body is left as it is, and its watcher never reports a failure.
//
// A body already watched keeps its watcher, and onFail is dropped: the first
// watcher records a failure before its onFail runs, and a second one wrapped
// around it would hear of the failure only after that onFail had let the
// transport close the body, which makes it ignore the failure.
func watchCallerBody(r *http.Request, onFail func()) *callerBody {
	if b, ok := r.Body.(*callerBody); ok {
		return b
	}
	b := &callerBody{ReadCloser: r.Body, onFail: onFail}
	if r.Body != nil {
		r.Body = b
	}
	return b
}

func (b *callerBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		b.fail(err)
	}
	return n, err
}

func (b *callerBody) fail(err error) {
	b.mu.Lock()
	first := b.err == nil && !b.closed
	if first {
		b.err = err
	}
	b.mu.Unlock()
	if first && b.onFail != nil {
		b.onFail()
	}
}

func (b *callerBody) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	return b.ReadCloser.Close()
}

// failure returns the first error reading the body, nil when it was read
// cleanly or not at all.
func (b *callerBody) failure() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

// writeCallerBodyError answers a forward whose request body could not be read
// the way the routed surfaces answer a body they could not decode: 413 for one
// past the size limit, 400 for any other.
func writeCallerBodyError(w http.ResponseWriter, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		apierror.WriteOpenAI(w, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request_error", "request_too_large")
		return
	}
	apierror.WriteOpenAI(w, http.StatusBadRequest, "invalid request body: "+err.Error(), "invalid_request_error", "invalid_request")
}
