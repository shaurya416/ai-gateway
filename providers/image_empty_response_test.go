package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestGenerateImage_SuccessWithoutImageFails is the image counterpart of the
// embeddings count check: a 2xx response that decodes to no usable image is a
// failed generation, not an answer. Each body below decodes without error on
// every listed provider, and before the check the provider returned it as an
// ImageResponse with no image in it, which the gateway served as a 200 and
// recorded as a generation the target had served.
//
// The bodies carry every surface's shape at once — an OpenAI-style "data" list
// and a Replicate prediction — so one stub answers all of them. gemini already
// refused this and stays listed as the reference behaviour.
func TestGenerateImage_SuccessWithoutImageFails(t *testing.T) {
	bodies := map[string]string{
		"empty result": `{"created":1,"data":[],"id":"pred-img","status":"succeeded","output":null}`,
		// An entry with neither a URL nor base64, and a prediction whose output
		// is an object rather than a URL.
		"no usable image": `{"created":1,"data":[{"revised_prompt":"a cat"}],"id":"pred-img","status":"succeeded","output":{"image":"https://example.com/a.png"}}`,
		// A failure reported in the body of a success status.
		"error envelope": `{"error":"upstream failed","id":"pred-img","status":"succeeded","output":""}`,
	}
	covered := []string{"azure_openai", "deepinfra", "gemini", "openai", "replicate", "together", "xai"}

	seen := make([]string, 0, len(covered))
	for _, tc := range statusConformanceCases() {
		if !slices.Contains(covered, tc.name) {
			continue
		}
		seen = append(seen, tc.name)
		for bodyName, body := range bodies {
			t.Run(tc.name+"/"+bodyName, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(body))
				}))
				defer srv.Close()

				model := tc.model
				if model == "" {
					model = "test-model"
				}
				ip, ok := tc.build(t, srv.URL).(ImageProvider)
				if !ok {
					t.Fatalf("%s does not implement ImageProvider", tc.name)
				}
				resp, err := ip.GenerateImage(context.Background(), core.ImageRequest{
					Model:  imageModelFor(tc.name, model),
					Prompt: "a cat",
				})
				if err == nil {
					t.Fatalf("GenerateImage() = %+v, nil error; want an error for a response with no image", resp)
				}
			})
		}
	}
	if len(seen) != len(covered) {
		t.Fatalf("covered providers found = %v, want all of %v", seen, covered)
	}
}
