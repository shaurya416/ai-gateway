package cohere

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestCohereProvider_Embed_RejectsMissingEmbeddings pins that a 200 from
// /v2/embed carrying fewer float embeddings than the request had texts is a
// failed call rather than a successful response holding fewer vectors than
// texts. v2 keys the vectors by type, so a body with no embeddings.float list —
// one carrying only another type, or none at all — is the same short answer.
func TestCohereProvider_Embed_RejectsMissingEmbeddings(t *testing.T) {
	cases := []struct {
		name  string
		input any
		body  string
	}{
		{"no embeddings", "hello", `{"id":"embed-1","embeddings":{"float":[]},"texts":[],"meta":{"billed_units":{"input_tokens":0}}}`},
		{"fewer embeddings than inputs", []string{"a", "b"}, `{"id":"embed-1","embeddings":{"float":[[0.1,0.2]]},"texts":["a"],"meta":{"billed_units":{"input_tokens":1}}}`},
		{"no float embeddings", "hello", `{"id":"embed-1","embeddings":{"int8":[[1,2]]},"texts":["hello"],"meta":{"billed_units":{"input_tokens":1}}}`},
		{"empty embeddings object", "hello", `{"id":"embed-1","embeddings":{},"texts":["hello"],"meta":{"billed_units":{"input_tokens":1}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			p, _ := New("test-key", srv.URL)
			resp, err := p.Embed(context.Background(), core.EmbeddingRequest{
				Model: "embed-english-v3.0",
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
