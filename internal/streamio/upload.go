package streamio

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// BoundUploadByIdleness re-bounds r's body by the gap between two reads rather
// than by the time the whole request takes, for a route that accepts uploads
// far larger than a JSON body.
//
// http.Server's ReadTimeout is the maximum duration for reading the ENTIRE
// request, body included, so on its own it caps an upload at whatever the
// caller can send inside it: a file still arriving steadily when it elapsed
// was cut off mid-body. Before each read of the body the connection's read
// deadline is pushed to ReadTimeout from now instead, so a body that keeps
// arriving is read to the end, and one that stops for ReadTimeout is ended
// exactly as before. The bound is the serving http.Server's own ReadTimeout,
// read from the request context; with none configured nothing changes.
//
// The returned release must run before the handler returns: a body read after
// that — the reverse proxy's transport can still be reading it — must not move
// a deadline that now belongs to the connection's next request. Once a read
// returns an error, end of body included, the deadline is no longer touched
// either, because net/http re-arms the connection for its own background read
// at that point.
func BoundUploadByIdleness(w http.ResponseWriter, r *http.Request) (release func()) {
	srv, _ := r.Context().Value(http.ServerContextKey).(*http.Server)
	if srv == nil || srv.ReadTimeout <= 0 || r.Body == nil || r.Body == http.NoBody {
		return func() {}
	}
	b := &idleUploadBody{
		ReadCloser: r.Body,
		controller: http.NewResponseController(w),
		idle:       srv.ReadTimeout,
	}
	r.Body = b
	return b.release
}

// idleUploadBody is the body BoundUploadByIdleness installs.
type idleUploadBody struct {
	io.ReadCloser
	controller *http.ResponseController
	idle       time.Duration

	// mu orders deadline updates against release: the body may be read from
	// the reverse proxy transport's goroutine while the handler returns.
	mu   sync.Mutex
	done bool
}

func (b *idleUploadBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if !b.done {
		// A writer that cannot set a read deadline leaves the server's own in
		// force, which is the behaviour this exists to relax, not a failure.
		_ = b.controller.SetReadDeadline(time.Now().Add(b.idle))
	}
	b.mu.Unlock()
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.release()
	}
	return n, err
}

func (b *idleUploadBody) release() {
	b.mu.Lock()
	b.done = true
	b.mu.Unlock()
}
