package ollamacloud

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestEmbed_RejectsMissingEmbeddings pins that a 200 from /api/embed carrying
// fewer embeddings than the request had inputs is a failed call rather than a
// successful response holding fewer vectors than texts.
func TestEmbed_RejectsMissingEmbeddings(t *testing.T) {
	cases := []struct {
		name  string
		input any
		body  string
	}{
		{"no embeddings", "hello", `{"model":"embed-model","embeddings":[]}`},
		{"fewer embeddings than inputs", []string{"a", "b"}, `{"model":"embed-model","embeddings":[[0.1,0.2]]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			p, err := New(testCloudAPIKey, srv.URL, []string{testCloudModel})
			if err != nil {
				t.Fatalf("New returned error: %v", err)
			}
			resp, err := p.Embed(context.Background(), core.EmbeddingRequest{
				Model: "embed-model",
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
