package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	ai21pkg "github.com/ferro-labs/ai-gateway/providers/ai21"
	azureopenaipkg "github.com/ferro-labs/ai-gateway/providers/azure_openai"
	coherepkg "github.com/ferro-labs/ai-gateway/providers/cohere"
	"github.com/ferro-labs/ai-gateway/providers/core"
	huggingfacepkg "github.com/ferro-labs/ai-gateway/providers/hugging_face"
	replicatepkg "github.com/ferro-labs/ai-gateway/providers/replicate"
	vertexaipkg "github.com/ferro-labs/ai-gateway/providers/vertex_ai"
)

// TestCallerRefusalsCarry400 pins the classification of refusals a provider
// makes itself, before any upstream call, because of what the caller sent: a
// parameter it cannot honour, a model it does not serve on that surface, or a
// model id that cannot be a path segment.
//
// Each of these was a bare error. A bare error carries no status, so the caller
// was answered 500 "internal error" with the reason withheld, the retry budget
// was spent asking again, and a pool mode offered the request to the next
// target as though this one had failed in transit.
func TestCallerRefusalsCarry400(t *testing.T) {
	var upstreamCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	two := 2
	dims := 256
	tests := []struct {
		name    string
		call    func(t *testing.T) error
		wantMsg string
	}{
		{
			name: "hugging face image with n above 1",
			call: func(t *testing.T) error {
				p, err := huggingfacepkg.New(testAPIKey, srv.URL)
				noErr(t, err)
				_, err = p.GenerateImage(context.Background(), core.ImageRequest{Model: "owner/model", Prompt: "a cat", N: &two})
				return err
			},
			wantMsg: "n=1",
		},
		{
			name: "hugging face model id with a dot segment",
			call: func(t *testing.T) error {
				p, err := huggingfacepkg.New(testAPIKey, srv.URL)
				noErr(t, err)
				_, err = p.Embed(context.Background(), core.EmbeddingRequest{Model: "owner/../model", Input: "hi"})
				return err
			},
			wantMsg: "invalid model path segment",
		},
		{
			name: "cohere embeddings with dimensions",
			call: func(t *testing.T) error {
				p, err := coherepkg.New(testAPIKey, srv.URL)
				noErr(t, err)
				_, err = p.Embed(context.Background(), core.EmbeddingRequest{Model: "embed-english-v3.0", Input: "hi", Dimensions: &dims})
				return err
			},
			wantMsg: "dimensions",
		},
		{
			name: "cohere embeddings with user",
			call: func(t *testing.T) error {
				p, err := coherepkg.New(testAPIKey, srv.URL)
				noErr(t, err)
				_, err = p.Embed(context.Background(), core.EmbeddingRequest{Model: "embed-english-v3.0", Input: "hi", User: "u-1"})
				return err
			},
			wantMsg: "user",
		},
		{
			name: "cohere embeddings with an unknown input_type",
			call: func(t *testing.T) error {
				p, err := coherepkg.New(testAPIKey, srv.URL)
				noErr(t, err)
				_, err = p.Embed(context.Background(), core.EmbeddingRequest{Model: "embed-english-v3.0", Input: "hi", InputType: "search_documents"})
				return err
			},
			wantMsg: "search_documents",
		},
		{
			name: "ai21 model id with a separator",
			call: func(t *testing.T) error {
				p, err := ai21pkg.New(testAPIKey, srv.URL)
				noErr(t, err)
				_, err = p.Complete(context.Background(), core.Request{
					Model:    "j2/ultra",
					Messages: []core.Message{{Role: core.RoleUser, Content: "hi"}},
				})
				return err
			},
			wantMsg: "invalid model",
		},
		{
			name: "azure openai deployment with a separator",
			call: func(t *testing.T) error {
				p, err := azureopenaipkg.New(testAPIKey, srv.URL, "chat", "2024-10-21")
				noErr(t, err)
				_, err = p.Embed(context.Background(), core.EmbeddingRequest{Model: "team/embedder", Input: "hi"})
				return err
			},
			wantMsg: "invalid deployment",
		},
		{
			name: "replicate image with a malformed size",
			call: func(t *testing.T) error {
				p, err := replicatepkg.New(testAPIKey, srv.URL, nil, []string{"owner/image-model"})
				noErr(t, err)
				_, err = p.GenerateImage(context.Background(), core.ImageRequest{Model: "owner/image-model", Prompt: "a cat", Size: "large"})
				return err
			},
			wantMsg: "invalid size",
		},
		{
			name: "replicate model id that is not owner/name",
			call: func(t *testing.T) error {
				p, err := replicatepkg.New(testAPIKey, srv.URL, nil, nil)
				noErr(t, err)
				_, err = p.GenerateImage(context.Background(), core.ImageRequest{Model: "owner/name/extra", Prompt: "a cat"})
				return err
			},
			wantMsg: "owner/name",
		},
		{
			name: "vertex ai embeddings on a chat model",
			call: func(t *testing.T) error {
				p, err := vertexaipkg.New(vertexaipkg.Options{ProjectID: "p", Region: "us-central1", APIKey: testAPIKey})
				noErr(t, err)
				_, err = p.Embed(context.Background(), core.EmbeddingRequest{Model: "gemini-2.0-flash", Input: "hi"})
				return err
			},
			wantMsg: "gemini-2.0-flash",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := upstreamCalls.Load()
			err := tt.call(t)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if got := core.ParseStatusCode(err); got != http.StatusBadRequest {
				t.Errorf("ParseStatusCode(%v) = %d, want %d", err, got, http.StatusBadRequest)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tt.wantMsg)
			}
			if n := upstreamCalls.Load() - before; n != 0 {
				t.Errorf("the refusal reached the upstream %d time(s)", n)
			}
		})
	}
}

// noErr fails the test when a provider constructor returned an error.
func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
}
