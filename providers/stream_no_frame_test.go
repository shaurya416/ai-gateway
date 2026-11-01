package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestNativeStream_NoFrameIsAFailure is the native-wire counterpart of the
// OpenAI-compatible reader's rule: a 200 that carries no stream — an HTML page
// from a catch-all route behind a misconfigured <PROVIDER>_BASE_URL, an empty
// body, nothing but keep-alives — delivers no chunk, and no completion is that
// short. These providers read their own event streams, and each ended such a
// body cleanly: the client got an empty stream, the request log wrote a
// zero-token success row and the circuit breaker counted a healthy call, while
// the same misconfiguration unstreamed failed to decode and was a failure.
func TestNativeStream_NoFrameIsAFailure(t *testing.T) {
	bodies := []struct {
		name        string
		contentType string
		body        string
	}{
		{"an HTML page", "text/html; charset=utf-8", "<!doctype html>\n<html><body>dashboard</body></html>\n"},
		{"an empty body", "text/event-stream", ""},
		{"keep-alives only", "text/event-stream", ": keep-alive\n\n: keep-alive\n\n"},
	}
	covered := []string{"anthropic", "cohere", "gemini"}

	var seen []string
	for _, tc := range statusConformanceCases() {
		if !slices.Contains(covered, tc.name) {
			continue
		}
		seen = append(seen, tc.name)
		for _, b := range bodies {
			t.Run(tc.name+"/"+b.name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", b.contentType)
					_, _ = io.WriteString(w, b.body)
				}))
				defer srv.Close()

				sp, ok := tc.build(t, srv.URL).(StreamProvider)
				if !ok {
					t.Fatalf("%s does not implement StreamProvider", tc.name)
				}
				ch, err := sp.CompleteStream(context.Background(), core.Request{
					Model:    "test-model",
					Messages: []core.Message{{Role: core.RoleUser, Content: "hi"}},
				})
				if err != nil {
					t.Fatalf("CompleteStream: %v", err)
				}
				var chunks []core.StreamChunk
				for c := range ch {
					chunks = append(chunks, c)
				}
				if len(chunks) != 1 || !errors.Is(chunks[0].Error, core.ErrNoStreamFrame) {
					t.Fatalf("got %d chunks (%+v), want exactly one ErrNoStreamFrame chunk: a stream with no frame must not read as a complete answer", len(chunks), chunks)
				}
			})
		}
	}
	slices.Sort(seen)
	if !slices.Equal(seen, covered) {
		t.Fatalf("covered providers %v, want %v: a native stream reader dropped out of the check", seen, covered)
	}
}
