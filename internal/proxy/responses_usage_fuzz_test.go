package proxy

import (
	"bytes"
	"io"
	"testing"
	"testing/iotest"

	"github.com/ferro-labs/ai-gateway/providers"
)

// FuzzResponsesUsageReader holds the usage tee to its two promises on any
// upstream body: the bytes the caller reads are the bytes the upstream sent,
// and the usage it extracts does not depend on how the transport happened to
// chunk them.
func FuzzResponsesUsageReader(f *testing.F) {
	f.Add([]byte(`{"id":"resp_1","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":20},"output_tokens":50,"output_tokens_details":{"reasoning_tokens":10},"total_tokens":150}}`), false)
	f.Add([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"usage\":null}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n"), true)
	f.Add([]byte("data: {\"response\":{\"usage\":{\"input_tokens\":1}}}\r\n\r\n"), true)
	f.Add([]byte(""), false)

	f.Fuzz(func(t *testing.T, body []byte, stream bool) {
		read := func(r io.Reader) ([]byte, providers.Usage) {
			var usage providers.Usage
			rc := newResponsesUsageReader(io.NopCloser(r), stream, &usage)
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if err := rc.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			return got, usage
		}
		whole, wholeUsage := read(bytes.NewReader(body))
		if !bytes.Equal(whole, body) {
			t.Fatalf("the tee altered the body")
		}
		split, splitUsage := read(iotest.OneByteReader(bytes.NewReader(body)))
		if !bytes.Equal(split, body) {
			t.Fatalf("the tee altered the body when read a byte at a time")
		}
		if wholeUsage != splitUsage {
			t.Fatalf("usage depends on chunking: whole %+v, byte at a time %+v", wholeUsage, splitUsage)
		}
	})
}
