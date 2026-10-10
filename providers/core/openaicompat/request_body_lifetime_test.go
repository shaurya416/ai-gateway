package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// lateReadTransport answers at once and leaves the request body unread until
// the test reads it. http.RoundTripper permits exactly that: a transport writes
// the body concurrently with reading the response, so when an upstream answers
// before it has consumed the whole request — an early 401 or 429, or a proxy
// that flushes its headers first — the body is still being sent after
// RoundTrip, and Client.Do, have returned.
type lateReadTransport struct {
	status int
	body   string
	sent   io.ReadCloser
}

func (t *lateReadTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.sent = r.Body
	return &http.Response{
		StatusCode: t.status,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(t.body)),
		Request:    r,
	}, nil
}

// TestRequestBodyOutlivesTheCall pins that the bytes a request body is read
// from belong to that request until the transport has finished with them.
//
// The body used to be a reader over a pooled buffer that the call handed back
// to the pool as it returned — on a stream, as soon as the response headers
// arrived. The next request marshaled its own JSON into that same memory, and
// the transport, still sending the first request, sent the second request's
// prompt to the first request's upstream.
func TestRequestBodyOutlivesTheCall(t *testing.T) {
	calls := []struct {
		name   string
		status int
		body   string
		call   func(ChatParams, core.Request) error
	}{
		{
			name:   "stream",
			status: http.StatusOK,
			body:   "data: [DONE]\n\n",
			call: func(p ChatParams, req core.Request) error {
				ch, err := PostStream(context.Background(), p, req)
				if err != nil {
					return err
				}
				for _, c := range collect(ch) {
					if c.Error != nil {
						return c.Error
					}
				}
				return nil
			},
		},
		{
			name:   "upstream answers before reading the body",
			status: http.StatusUnauthorized,
			body:   `{"error":{"message":"invalid api key"}}`,
			call: func(p ChatParams, req core.Request) error {
				if _, err := PostChat(context.Background(), p, req); err == nil {
					return fmt.Errorf("PostChat on a 401 returned no error")
				}
				return nil
			},
		},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			// Several rounds, because whether the next marshal is handed the
			// same buffer is up to sync.Pool, which the race detector randomises.
			for round := range 16 {
				tr := &lateReadTransport{status: tc.status, body: tc.body}
				prompt := fmt.Sprintf("prompt of request %d ", round) + strings.Repeat("a", 2048)
				p := ChatParams{
					HTTPClient: &http.Client{Transport: tr},
					URL:        "http://upstream.invalid/v1/chat/completions",
					Provider:   "test",
					Label:      "test",
				}
				req := core.Request{Model: "m", Messages: []core.Message{{Role: core.RoleUser, Content: prompt}}}
				if err := tc.call(p, req); err != nil {
					t.Fatal(err)
				}

				// Another request marshals its body while the first is still
				// on the wire.
				other, _, _, err := core.JSONBodyReader(core.Request{
					Model:    "another-callers-model",
					Messages: []core.Message{{Role: core.RoleUser, Content: strings.Repeat("b", 2048)}},
				})
				if err != nil {
					t.Fatal(err)
				}
				_ = other

				sent, err := io.ReadAll(tr.sent)
				if err != nil {
					t.Fatalf("round %d: reading the request body: %v", round, err)
				}
				var got core.Request
				if err := json.Unmarshal(sent, &got); err != nil {
					t.Fatalf("round %d: the body sent upstream is not the request's JSON (%v): %.120s", round, err, sent)
				}
				if got.Model != "m" || len(got.Messages) != 1 || got.Messages[0].Content != prompt {
					t.Fatalf("round %d: the body sent upstream is not the request that was made: model %q, %.80s", round, got.Model, sent)
				}
			}
		})
	}
}
