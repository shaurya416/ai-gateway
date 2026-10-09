package gemini

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// geminiErrorEnvelope returns Gemini's own error body from testdata, the shape
// a non-success status carries and a failed stream sends as a data frame.
func geminiErrorEnvelope(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "error.401.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(bytes.TrimSpace(body))
}

func streamGemini(t *testing.T, sse string) []core.StreamChunk {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer srv.Close()

	p, _ := New("test-key", srv.URL)
	ch, err := p.CompleteStream(context.Background(), core.Request{
		Model:    "gemini-2.5-flash",
		Messages: []core.Message{{Role: core.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	var chunks []core.StreamChunk
	for c := range ch {
		chunks = append(chunks, c)
	}
	return chunks
}

// TestGeminiProvider_CompleteStream_ErrorFrameFailsStream verifies a mid-stream
// error envelope ends the stream with an error instead of decoding to an empty
// delta and letting the stream close as a success at EOF.
func TestGeminiProvider_CompleteStream_ErrorFrameFailsStream(t *testing.T) {
	envelope := geminiErrorEnvelope(t)

	tests := []struct {
		name        string
		sse         string
		wantContent int
	}{
		{
			name: "after content",
			sse: `data: {"responseId":"r1","candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}]}` + "\n\n" +
				"data: " + envelope + "\n\n",
			wantContent: 1,
		},
		{
			name:        "first frame",
			sse:         "data: " + envelope + "\n\n",
			wantContent: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := streamGemini(t, tt.sse)

			if len(chunks) != tt.wantContent+1 {
				t.Fatalf("chunks = %#v, want %d content chunk(s) then one error", chunks, tt.wantContent)
			}
			for i, c := range chunks[:tt.wantContent] {
				if c.Error != nil {
					t.Fatalf("chunk %d error = %v, want content", i, c.Error)
				}
			}
			last := chunks[len(chunks)-1]
			if last.Error == nil {
				t.Fatalf("terminal chunk = %#v, want the stream to fail on the error frame", last)
			}
			var statusErr *core.HTTPStatusError
			if !errors.As(last.Error, &statusErr) {
				t.Fatalf("error = %T %v, want *core.HTTPStatusError", last.Error, last.Error)
			}
			if statusErr.StatusCode != http.StatusBadRequest || statusErr.Code != "INVALID_ARGUMENT" {
				t.Errorf("status/code = %d/%q, want 400/INVALID_ARGUMENT", statusErr.StatusCode, statusErr.Code)
			}
			if !strings.Contains(statusErr.Message, "API key not valid") {
				t.Errorf("message = %q, want the upstream's own text", statusErr.Message)
			}
		})
	}
}

// TestGeminiProvider_CompleteStream_NullErrorIsHealthy verifies a frame whose
// error field is null or empty is decoded as an ordinary chunk.
func TestGeminiProvider_CompleteStream_NullErrorIsHealthy(t *testing.T) {
	sse := `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}],"error":null}` + "\n\n" +
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"!"}]},"finishReason":"STOP"}],"error":{}}` + "\n\n"

	chunks := streamGemini(t, sse)

	if len(chunks) != 2 {
		t.Fatalf("chunks = %#v, want two content chunks", chunks)
	}
	for i, c := range chunks {
		if c.Error != nil {
			t.Fatalf("chunk %d error = %v, want none", i, c.Error)
		}
	}
	if got := chunks[1].Choices[0].FinishReason; got != core.FinishReasonStop {
		t.Errorf("finish_reason = %q, want stop", got)
	}
}
