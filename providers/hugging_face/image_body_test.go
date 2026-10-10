package huggingface

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestHuggingFaceProvider_GenerateImage_RefusesNonImageBody covers a 200 whose
// body is not an image. The model route runs the model's own pipeline, so a
// model that does not generate images answers 200 with JSON, and that body (or
// an empty one) used to be base64-encoded and served as the generated image.
func TestHuggingFaceProvider_GenerateImage_RefusesNonImageBody(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfake-image-bytes")
	tests := []struct {
		name        string
		contentType string
		body        []byte
		wantImage   bool
	}{
		{name: "text-generation JSON", contentType: "application/json", body: []byte(`[{"generated_text":"a cat sitting on a mat"}]`)},
		{name: "empty body", contentType: "image/jpeg", body: nil},
		{name: "JSON without a content type", body: []byte(`{"generated_text":"a cat"}`)},
		{name: "declared image type", contentType: "image/tiff", body: []byte("II*\x00tiff-bytes"), wantImage: true},
		{name: "sniffed image, generic type", contentType: "application/octet-stream", body: png, wantImage: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.contentType != "" {
					w.Header().Set("Content-Type", tt.contentType)
				} else {
					w.Header()["Content-Type"] = nil
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(tt.body)
			}))
			defer srv.Close()

			p, _ := New(testAPIKey, srv.URL)
			resp, err := p.GenerateImage(context.Background(), core.ImageRequest{
				Model:  "openai-community/gpt2",
				Prompt: "a cat",
			})
			if !tt.wantImage {
				if err == nil {
					t.Fatalf("GenerateImage served a non-image body as an image: %+v", resp)
				}
				return
			}
			if err != nil {
				t.Fatalf("GenerateImage() error: %v", err)
			}
			if got, want := resp.Data[0].B64JSON, base64.StdEncoding.EncodeToString(tt.body); got != want {
				t.Errorf("B64JSON = %q, want %q", got, want)
			}
		})
	}
}
