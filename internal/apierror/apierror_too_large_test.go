package apierror

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestWriteRouteError_UpstreamTooLargeIsTheCallersError holds an upstream 413 to
// what it says: the request the gateway translated from the caller's was too
// large for the provider, and only the caller can make it smaller.
//
// It fell through to the catch-all and was answered 502 upstream_error, "the
// upstream provider returned no usable response". Every OpenAI SDK retries a
// 5xx, so each retry resent the same oversized request for the same refusal, and
// the caller was never told what to change — while the identical condition sent
// as a 400 (the context-length envelope core.IsContextLengthError reads at either
// status) reached the caller as its own error with the provider's account.
func TestWriteRouteError_UpstreamTooLargeIsTheCallersError(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantMessage string
	}{
		{
			name:        "context length envelope",
			body:        `{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`,
			wantMessage: "This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.",
		},
		{
			// A size limit enforced by a proxy in front of the provider answers
			// in no envelope the gateway reads; the status text stands in.
			name:        "byte limit with no JSON envelope",
			body:        `<html><body>413 Request Entity Too Large</body></html>`,
			wantMessage: "Request Entity Too Large",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := fmt.Errorf("all providers failed: %w", core.APIError("openai", http.StatusRequestEntityTooLarge, []byte(tt.body)))

			w := httptest.NewRecorder()
			WriteRouteError(w, err)

			if w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413: an oversized request is the caller's to shrink, and a 5xx is retried unchanged", w.Code)
			}
			var body struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
					Code    string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v: %s", err, w.Body.String())
			}
			if body.Error.Type != errTypeInvalidRequest || body.Error.Code != "request_too_large" {
				t.Fatalf("type/code = %q/%q, want %q/request_too_large", body.Error.Type, body.Error.Code, errTypeInvalidRequest)
			}
			if body.Error.Message != tt.wantMessage {
				t.Fatalf("message = %q, want %q", body.Error.Message, tt.wantMessage)
			}
			if got := w.Header().Get("Retry-After"); got != "" {
				t.Fatalf("Retry-After = %q, want none: waiting does not shrink a request", got)
			}
		})
	}
}
