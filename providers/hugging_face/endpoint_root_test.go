package huggingface

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestNew_BareHostEndpointAdoptsVersionSegment pins HUGGING_FACE_ENDPOINT to
// the base-URL rule every other provider follows: a value with no path is a
// host, not an API root, so it resolves to the version segment the default
// root ends in, and the task routes hang off the host itself.
//
// The endpoint was stored verbatim, so a host — the router's, or a dedicated
// Inference Endpoint's — sent chat to /chat/completions and discovery to
// /models, neither of which the OpenAI-compatible surface serves; only the
// task routes, which strip /v1 back off, reached anything.
func TestNew_BareHostEndpointAdoptsVersionSegment(t *testing.T) {
	for _, suffix := range []string{"", "/"} {
		t.Run("host"+suffix, func(t *testing.T) {
			var (
				mu    sync.Mutex
				paths []string
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"message":"path recorder"}}`))
			}))
			defer srv.Close()

			p, err := New(testAPIKey, srv.URL+suffix)
			if err != nil {
				t.Fatalf("New(%q): %v", srv.URL+suffix, err)
			}
			if want := srv.URL + "/v1"; p.BaseURL() != want {
				t.Errorf("BaseURL() = %q, want %q", p.BaseURL(), want)
			}

			ctx := context.Background()
			chat := core.Request{Model: "owner/chat", Messages: []core.Message{{Role: core.RoleUser, Content: "hi"}}}
			_, _ = p.Complete(ctx, chat)
			_, _ = p.CompleteStream(ctx, chat)
			_, _ = p.DiscoverModels(ctx)
			_, _ = p.Embed(ctx, core.EmbeddingRequest{Model: "owner/embed", Input: "hi"})
			_, _ = p.GenerateImage(ctx, core.ImageRequest{Model: "owner/image", Prompt: "a cat"})

			mu.Lock()
			got := slices.Clone(paths)
			mu.Unlock()
			slices.Sort(got)
			want := []string{
				"/hf-inference/models/owner/embed/pipeline/feature-extraction",
				"/hf-inference/models/owner/image",
				"/v1/chat/completions",
				"/v1/chat/completions",
				"/v1/models",
			}
			if !slices.Equal(got, want) {
				t.Errorf("requested paths = %q, want %q", got, want)
			}
		})
	}
}

// TestNew_EndpointWithPathIsUsedVerbatim keeps the other half of the rule: an
// endpoint that carries a path is the operator's own root and gains nothing.
func TestNew_EndpointWithPathIsUsedVerbatim(t *testing.T) {
	p, err := New(testAPIKey, "https://proxy.example.com/hf/v1/")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got, want := p.BaseURL(), "https://proxy.example.com/hf/v1"; got != want {
		t.Errorf("BaseURL() = %q, want %q", got, want)
	}
}

// TestNew_RefusesEndpointWithQuery: an operation path is appended to the root,
// so a query string would bury it in a query value — refused at construction,
// as it is for every provider that resolves its root.
func TestNew_RefusesEndpointWithQuery(t *testing.T) {
	for _, endpoint := range []string{
		"https://router.huggingface.co/v1?token=x",
		"https://router.huggingface.co/v1#frag",
	} {
		if _, err := New(testAPIKey, endpoint); err == nil {
			t.Errorf("New(%q) succeeded; a query or fragment cannot carry an appended operation path", endpoint)
		}
	}
}
