package cohere

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestNew_BaseURLIsTheAPIRoot pins the shape COHERE_BASE_URL is written in:
// the API root, version segment included, used verbatim. The one net: a bare
// host is not an API root, so it adopts the /v2 the default root ends in.
func TestNew_BaseURLIsTheAPIRoot(t *testing.T) {
	for base, want := range map[string]string{
		"":                                     "https://api.cohere.com/v2",
		"https://api.cohere.com":               "https://api.cohere.com/v2",
		"https://api.cohere.com/":              "https://api.cohere.com/v2",
		"https://api.cohere.com/v2":            "https://api.cohere.com/v2",
		"https://proxy.example.com/cohere/v2/": "https://proxy.example.com/cohere/v2",
		"https://proxy.example.com/cohere":     "https://proxy.example.com/cohere",
	} {
		p, err := New("test-key", base)
		if err != nil {
			t.Fatalf("New(_, %q) error: %v", base, err)
		}
		if got := p.BaseURL(); got != want {
			t.Errorf("New(_, %q).BaseURL() = %q, want %q", base, got, want)
		}
	}
}

// TestEverySurfaceHangsOffTheAPIRoot drives chat, streaming, embeddings and
// rerank against one recorded base and asserts the paths that leave the
// process: each surface appends only its operation path to the resolved root.
// A host-only base still reaches /v2, and a path-mounted base is taken as
// written, with no version segment added beneath it.
func TestEverySurfaceHangsOffTheAPIRoot(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"message":"path recorder"}`))
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name string
		base string
		root string
	}{
		{"host only", srv.URL, "/v2"},
		{"path-mounted with version", srv.URL + "/proxy/v2", "/proxy/v2"},
		{"path-mounted without version", srv.URL + "/proxy", "/proxy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			paths = nil
			mu.Unlock()

			p, err := New("test-key", tc.base)
			if err != nil {
				t.Fatalf("New(_, %q) error: %v", tc.base, err)
			}
			ctx := context.Background()
			chat := core.Request{Model: "command-r", Messages: []core.Message{{Role: core.RoleUser, Content: "hi"}}}
			_, _ = p.Complete(ctx, chat)
			_, _ = p.CompleteStream(ctx, chat)
			_, _ = p.Embed(ctx, core.EmbeddingRequest{Model: "embed-v4.0", Input: "hi"})
			_, _ = p.Rerank(ctx, core.RerankRequest{Model: "rerank-v3.5", Query: "q", Documents: []string{"d"}})

			mu.Lock()
			got := append([]string(nil), paths...)
			mu.Unlock()
			sort.Strings(got)
			want := []string{tc.root + "/chat", tc.root + "/chat", tc.root + "/embed", tc.root + "/rerank"}
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("base %q: upstream paths = %v, want %v", tc.base, got, want)
			}
		})
	}
}
