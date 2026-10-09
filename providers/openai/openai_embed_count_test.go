package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestOpenAIProvider_Embed_RejectsMissingEmbeddings pins that a 2xx answer
// carrying fewer embeddings than the request had inputs is a failure. The SDK
// decodes an error envelope on a 200, or a short list, without error, so it was
// returned as a successful response holding fewer vectors than the caller sent
// texts, and the target was recorded as healthy.
func TestOpenAIProvider_Embed_RejectsMissingEmbeddings(t *testing.T) {
	cases := []struct {
		name  string
		input any
		body  string
	}{
		{
			name:  "error envelope on a 200",
			input: "hello",
			body:  `{"error":{"message":"upstream overloaded","code":502}}`,
		},
		{
			name:  "fewer embeddings than inputs",
			input: []string{"a", "b"},
			body: `{"object":"list","model":"m","usage":{"prompt_tokens":1,"total_tokens":1},` +
				`"data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			p, err := New("sk-test", srv.URL)
			if err != nil {
				t.Fatalf("New(): %v", err)
			}
			resp, err := p.Embed(context.Background(), core.EmbeddingRequest{
				Model: "text-embedding-3-small",
				Input: tc.input,
			})
			if err == nil {
				t.Fatalf("Embed() = %+v, want an error for a short answer", resp)
			}
			if !strings.Contains(err.Error(), "embeddings for") {
				t.Errorf("error = %v, want it to name the embedding count", err)
			}
		})
	}
}
