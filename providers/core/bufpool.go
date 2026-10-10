package core

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
)

// bufPool holds reusable bytes.Buffer instances for JSON marshaling on the
// provider hot path. Using a pool avoids growing a fresh encoding buffer on
// every request; the encoded bytes are copied out before the buffer is put
// back, so nothing outside this file ever holds pooled memory.
var bufPool = sync.Pool{
	New: func() any {
		return bytes.NewBuffer(make([]byte, 0, 2048))
	},
}

// MarshalJSON encodes v to JSON using a pooled buffer and returns the
// resulting byte slice. The caller owns the returned slice; the underlying
// buffer is returned to the pool.
func MarshalJSON(v any) ([]byte, error) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()

	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		buf.Reset()
		bufPool.Put(buf)
		return nil, err
	}

	// json.Encoder.Encode appends a trailing newline; trim it to match
	// json.Marshal behaviour.
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}

	// Copy out so we can return the buffer to the pool.
	out := make([]byte, len(b))
	copy(out, b)

	buf.Reset()
	bufPool.Put(buf)
	return out, nil
}

// JSONBodyReader encodes v to JSON and returns an io.Reader over the result
// along with the content length. The reader is a *bytes.Reader, so
// http.NewRequest sets Content-Length and GetBody from it.
//
// The bytes the reader serves belong to that request alone: the pooled buffer
// is used only to encode, and the result is copied out of it before the buffer
// goes back to the pool. A request body has to stay intact until the transport
// is done with it, and that can be after Client.Do has returned — the transport
// writes the body while it reads the response, and keeps writing when an
// upstream answers before reading the whole request (an early 401 or 429, or a
// proxy that flushes its headers first). A reader over the pooled buffer itself
// was rewritten by the next request's JSON while still on the wire, so one
// caller's prompt was sent to another caller's upstream.
//
// release is retained for callers and has nothing left to return; calling it
// is harmless.
func JSONBodyReader(v any) (body io.Reader, contentLen int, release func(), err error) {
	b, err := MarshalJSON(v)
	if err != nil {
		return nil, 0, nil, err
	}
	return bytes.NewReader(b), len(b), releaseNothing, nil
}

// releaseNothing is the release func JSONBodyReader returns: the body it built
// owns its bytes, so there is no buffer to give back.
func releaseNothing() {}
