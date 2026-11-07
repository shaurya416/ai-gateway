package openaicompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"testing/iotest"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// drainStream renders every chunk StreamSSE delivers for body, errors as text.
func drainStream(t *testing.T, body io.Reader) []string {
	t.Helper()
	var seen []string
	for chunk := range StreamSSE(t.Context(), io.NopCloser(body)) {
		errText := ""
		if chunk.Error != nil {
			errText = chunk.Error.Error()
		}
		encoded, err := json.Marshal(chunk)
		if err != nil {
			t.Fatalf("a delivered chunk does not encode: %v", err)
		}
		seen = append(seen, fmt.Sprintf("%s err=%q", encoded, errText))
	}
	return seen
}

// FuzzStreamSSE holds StreamSSE to its godoc on any upstream body: it always
// ends, at most one chunk carries an error and it is the last, a body that
// delivered nothing and sent no [DONE] ends with ErrNoStreamFrame, and what is
// delivered does not depend on how the transport chunked the bytes.
func FuzzStreamSSE(f *testing.F) {
	f.Add([]byte("data: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	f.Add([]byte("data:{\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_use\"}]}\r\n\r\n"))
	f.Add([]byte(": keep-alive\n\ndata: {\"error\":{\"type\":\"server_error\",\"message\":\"boom\"}}\n\ndata: {\"choices\":[]}\n\n"))
	f.Add([]byte("data: {\"error\":\"rate limited\"}\n\n"))
	f.Add([]byte("data: {\"choices\":[{\"delta\":{\"content\":[{\"type\":\"thinking\",\"thinking\":[{\"type\":\"text\",\"text\":\"r\"}]},{\"type\":\"text\",\"text\":\"t\"}]}}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n"))
	f.Add([]byte("<html>bad gateway</html>"))
	f.Add([]byte(""))
	f.Add([]byte("data:[DONE]"))

	f.Fuzz(func(t *testing.T, body []byte) {
		whole := drainStream(t, bytes.NewReader(body))
		split := drainStream(t, iotest.OneByteReader(bytes.NewReader(body)))
		if fmt.Sprint(whole) != fmt.Sprint(split) {
			t.Fatalf("chunking changed the stream:\nwhole: %q\nsplit: %q", whole, split)
		}

		var chunks []core.StreamChunk
		for chunk := range StreamSSE(t.Context(), io.NopCloser(bytes.NewReader(body))) {
			chunks = append(chunks, chunk)
		}
		if len(chunks) == 0 {
			// Only the [DONE] sentinel ends a stream that delivered nothing
			// without ErrNoStreamFrame: it is the upstream saying so.
			lines, _ := core.SSEDataLines(bytes.NewReader(body))
			for data := range lines {
				if data == core.SSEDone {
					return
				}
			}
			t.Fatalf("the stream closed without a chunk, an error or [DONE]")
		}
		for i, chunk := range chunks[:len(chunks)-1] {
			if chunk.Error != nil {
				t.Fatalf("chunk %d of %d carries an error, but an error ends the stream: %v", i, len(chunks), chunk.Error)
			}
		}
	})
}
