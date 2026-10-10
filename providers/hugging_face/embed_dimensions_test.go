package huggingface

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestEmbed_RefusesDimensions pins that a requested output dimension is
// refused rather than ignored. The feature-extraction task takes inputs,
// normalize, prompt_name, truncate and truncation_direction and nothing that
// sets a size (huggingface_hub FeatureExtractionInput), so the model always
// answers in its native dimension. Ignoring the field served a vector of a
// size the caller did not ask for under a 200 — one that fails, or silently
// mismatches, in the index it was requested for.
func TestEmbed_RefusesDimensions(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[0.1,0.2,0.3]`))
	}))
	defer srv.Close()

	p, err := New(testAPIKey, srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dims := 2
	resp, err := p.Embed(context.Background(), core.EmbeddingRequest{
		Model:      "test-embed",
		Input:      "hello",
		Dimensions: &dims,
	})
	if err == nil {
		t.Fatalf("Embed served a %d-dimension vector for dimensions=%d; want a 400 refusal",
			len(resp.Data[0].Embedding), dims)
	}
	if got := core.ParseStatusCode(err); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (err: %v)", got, err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("upstream called %d times; the refusal must precede the call", n)
	}
}
