package azureopenai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestAzureOpenAIProvider_Embed_InputRefusalsAre400 pins that an embeddings input
// this provider cannot embed — token ids included, the shape LangChain's
// OpenAIEmbeddings sends by default — is the caller's 400, refused before any
// upstream call. As a bare error it answered 500, was retried as a transport
// failure, and was offered to every other target in the pool.
func TestAzureOpenAIProvider_Embed_InputRefusalsAre400(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p, err := New("test-key", srv.URL, "gpt-4o", "2024-10-21")
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	for name, input := range map[string]any{
		"nil":                nil,
		"empty array":        []any{},
		"non-string element": []any{"ok", float64(42)},
		"token ids":          []any{[]any{float64(9906), float64(1917)}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.Embed(context.Background(), core.EmbeddingRequest{Model: "text-embedding-3-small", Input: input})
			if got := core.ParseStatusCode(err); got != http.StatusBadRequest {
				t.Errorf("Embed() error = %v (status %d), want a 400", err, got)
			}
		})
	}
	if called {
		t.Error("refused input reached the upstream")
	}
}
