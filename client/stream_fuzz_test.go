package client

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"testing/iotest"
)

// readAllFrom runs the stream reader over r and renders what a caller sees:
// every chunk, then how the stream ended.
func readAllFrom(r io.Reader) []string {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), maxEventBytes+1)
	scanner.Split((&lineSplitter{}).split)
	s := &ChatCompletionStream{ctx: context.Background(), body: io.NopCloser(r), scanner: scanner}
	var seen []string
	for s.Next() {
		seen = append(seen, fmt.Sprintf("chunk %+v", s.Current()))
	}
	var streamErr *StreamError
	switch err := s.Err(); {
	case err == nil:
		seen = append(seen, "done")
	case errors.As(err, &streamErr):
		seen = append(seen, fmt.Sprintf("stream error %s/%s: %s", streamErr.Type, streamErr.Code, streamErr.Message))
	case errors.Is(err, ErrStreamTruncated):
		seen = append(seen, "truncated")
	default:
		seen = append(seen, "error "+err.Error())
	}
	return seen
}

// FuzzStreamReader feeds arbitrary bytes to the event-stream reader. Whatever
// the bytes, the reader ends in one defined way, and what it reports does not
// depend on how the connection chunked them: a CR that ends one read and the LF
// that begins the next are one line ending, as they are when they arrive
// together.
func FuzzStreamReader(f *testing.F) {
	f.Add([]byte("data: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	f.Add([]byte(": comment\r\ndata: {\"id\":\"a\"}\r\n\r\ndata: [DONE]\r\n\r\n"))
	f.Add([]byte("data: {\"id\":\"a\"}\r\rdata:[DONE]\r\r"))
	f.Add([]byte("event: error\ndata: {\"error\":{\"message\":\"boom\",\"type\":\"stream_error\"}}\n\n"))
	f.Add([]byte("data: {\"id\":\n"))
	f.Add([]byte("data: {\"id\":\"a\"}\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		whole := readAllFrom(bytes.NewReader(data))
		split := readAllFrom(iotest.OneByteReader(bytes.NewReader(data)))
		if fmt.Sprint(whole) != fmt.Sprint(split) {
			t.Fatalf("chunking changed the stream:\nwhole: %q\nsplit: %q", whole, split)
		}
	})
}
