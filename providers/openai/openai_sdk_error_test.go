package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	core "github.com/ferro-labs/ai-gateway/providers/core"
	oai "github.com/openai/openai-go"
)

// TestSDKSurfacesClassifyErrorsLikeChat holds embeddings and image generation,
// which go through openai-go, to the classification the raw-HTTP chat path
// gives the same upstream answer. The gateway decides failover and circuit
// breaking on the typed error's code and type as well as its status — a
// context-window refusal is told apart from every other 400 by them — so an
// adapter that keeps only the status and the message answers the same refusal
// differently depending on the surface it arrived on.
func TestSDKSurfacesClassifyErrorsLikeChat(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		body          string
		contextLength bool
	}{
		{
			name:   "context window refusal with no code",
			status: http.StatusBadRequest,
			body: `{"error":{"message":"This model's maximum context length is 8192 tokens, however you requested 9000 tokens (9000 in your prompt; 0 for the completion). Please reduce your prompt; or completion length.",` +
				`"type":"invalid_request_error","param":null,"code":null}}`,
			contextLength: true,
		},
		{
			name:          "context window refusal with its code",
			status:        http.StatusBadRequest,
			body:          `{"error":{"message":"input too long","type":"invalid_request_error","param":"input","code":"context_length_exceeded"}}`,
			contextLength: true,
		},
		{
			name:          "context window refusal sent as 413",
			status:        http.StatusRequestEntityTooLarge,
			body:          `{"error":{"message":"input too long","type":"invalid_request_error","code":"context_length_exceeded"}}`,
			contextLength: true,
		},
		{
			name:          "Gemini-shaped refusal behind OPENAI_BASE_URL",
			status:        http.StatusBadRequest,
			body:          `{"error":{"code":400,"message":"The input token count (1500000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`,
			contextLength: true,
		},
		{
			name:   "an ordinary 400 stays an ordinary 400",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"Invalid value for 'dimensions'","type":"invalid_request_error","param":"dimensions","code":"invalid_value"}}`,
		},
		{
			name:   "validation detail from a compatible server",
			status: http.StatusUnprocessableEntity,
			body:   `{"detail":[{"loc":["body","input"],"msg":"field required","type":"missing"}]}`,
		},
		{
			name:   "string error envelope",
			status: http.StatusTooManyRequests,
			body:   `{"error":"rate limited"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			p, err := New("sk-test", srv.URL)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx := context.Background()

			_, chatErr := p.Complete(ctx, core.Request{
				Model:    "gpt-4o",
				Messages: []core.Message{{Role: core.RoleUser, Content: "hi"}},
			})
			want := statusErrorOf(t, "chat", chatErr)
			if got := core.IsContextLengthError(chatErr); got != tc.contextLength {
				t.Fatalf("chat: IsContextLengthError = %v, want %v", got, tc.contextLength)
			}

			_, embedErr := p.Embed(ctx, core.EmbeddingRequest{Model: "text-embedding-3-small", Input: "hi"})
			_, imageErr := p.GenerateImage(ctx, core.ImageRequest{Model: "dall-e-3", Prompt: "a cat"})
			for surface, err := range map[string]error{"embeddings": embedErr, "images": imageErr} {
				got := statusErrorOf(t, surface, err)
				if got.StatusCode != want.StatusCode || got.Message != want.Message ||
					got.Code != want.Code || got.Type != want.Type {
					t.Errorf("%s: classified as {status %d, message %q, code %q, type %q}; "+
						"chat classifies the same answer as {status %d, message %q, code %q, type %q}",
						surface, got.StatusCode, got.Message, got.Code, got.Type,
						want.StatusCode, want.Message, want.Code, want.Type)
				}
				if got := core.IsContextLengthError(err); got != tc.contextLength {
					t.Errorf("%s: IsContextLengthError = %v, want %v", surface, got, tc.contextLength)
				}
			}
		})
	}
}

// TestSDKErrorWithoutBodyKeepsDecodedEnvelope covers an SDK error whose body is
// no longer on the response: the code and type the SDK decoded from the
// envelope still reach the typed error, so the context-window refusal is still
// recognised.
func TestSDKErrorWithoutBodyKeepsDecodedEnvelope(t *testing.T) {
	apiErr := &oai.Error{
		Code:       "context_length_exceeded",
		Message:    "input too long",
		Type:       "invalid_request_error",
		StatusCode: http.StatusBadRequest,
		Response:   &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}},
	}
	err := sdkError(apiErr)
	got := statusErrorOf(t, "sdk", err)
	if got.StatusCode != http.StatusBadRequest || got.Message != "input too long" ||
		got.Code != "context_length_exceeded" || got.Type != "invalid_request_error" {
		t.Errorf("sdkError = {status %d, message %q, code %q, type %q}", got.StatusCode, got.Message, got.Code, got.Type)
	}
	if !core.IsContextLengthError(err) {
		t.Error("IsContextLengthError = false for a context_length_exceeded refusal")
	}
}

func statusErrorOf(t *testing.T, surface string, err error) *core.HTTPStatusError {
	t.Helper()
	var statusErr *core.HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("%s: error %v (%T) carries no *core.HTTPStatusError", surface, err, err)
	}
	return statusErr
}
