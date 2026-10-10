package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestOpenAIProvider_Embed_ForwardsTokenIDInput pins that token-id input — an
// array of token ids, or an array of such arrays, both members of the OpenAI
// embeddings input union — reaches OpenAI as the integers it was sent as.
// LangChain's OpenAIEmbeddings sends this shape by default. It was refused as
// text-only with a bare error: answered 500, retried, and offered to every other
// target, without any upstream ever being asked.
func TestOpenAIProvider_Embed_ForwardsTokenIDInput(t *testing.T) {
	cases := []struct {
		name      string
		input     any
		wantInput string
		vectors   int
	}{
		{
			name:      "one input as a token array",
			input:     []any{float64(9906), float64(1917)},
			wantInput: `[9906,1917]`,
			vectors:   1,
		},
		{
			name:      "two inputs as token arrays",
			input:     []any{[]any{float64(9906), float64(1917)}, []any{float64(15339)}},
			wantInput: `[[9906,1917],[15339]]`,
			vectors:   2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotInput json.RawMessage
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var req struct {
					Input json.RawMessage `json:"input"`
				}
				_ = json.Unmarshal(body, &req)
				gotInput = req.Input
				data := make([]map[string]any, tc.vectors)
				for i := range data {
					data[i] = map[string]any{"object": "embedding", "index": i, "embedding": []float64{0.1, 0.2}}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"object": "list",
					"model":  "text-embedding-3-small",
					"data":   data,
					"usage":  map[string]int{"prompt_tokens": 3, "total_tokens": 3},
				})
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
			if err != nil {
				t.Fatalf("Embed() error = %v, want token-id input forwarded", err)
			}
			if string(gotInput) != tc.wantInput {
				t.Errorf("upstream input = %s, want %s", gotInput, tc.wantInput)
			}
			if len(resp.Data) != tc.vectors {
				t.Errorf("got %d embeddings, want %d", len(resp.Data), tc.vectors)
			}
		})
	}
}

// TestOpenAIProvider_Embed_MalformedTokenInputIs400 pins that token-id input
// the union cannot carry is the caller's 400, refused before any upstream call.
func TestOpenAIProvider_Embed_MalformedTokenInputIs400(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p, err := New("sk-test", srv.URL)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	for name, input := range map[string]any{
		"fractional token id":  []any{float64(1.5)},
		"negative token id":    []any{float64(-1)},
		"text inside tokens":   []any{float64(1), "two"},
		"empty token array":    []any{[]any{}},
		"scalar in token list": []any{[]any{float64(1)}, float64(2)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.Embed(context.Background(), core.EmbeddingRequest{Model: "text-embedding-3-small", Input: input})
			if got := core.ParseStatusCode(err); got != http.StatusBadRequest {
				t.Errorf("Embed() error = %v (status %d), want a 400", err, got)
			}
		})
	}
	if called {
		t.Error("malformed input reached the upstream")
	}
}
