package together

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestTogetherProvider_GenerateImage_TranslatesSizeAndFormat pins the request
// Together's image API is sent. Its schema takes the dimensions as width and
// height and response_format as "base64" or "url"; it has no size field and no
// "b64_json" value. Forwarded in OpenAI's spelling, the size a caller asked for
// never reached the model and a base64 request named a format the API does not
// define.
func TestTogetherProvider_GenerateImage_TranslatesSizeAndFormat(t *testing.T) {
	cases := []struct {
		name       string
		size       string
		format     string
		wantWidth  int
		wantHeight int
		wantFormat string
	}{
		{name: "size and b64_json", size: "1024x768", format: core.ImageResponseFormatB64JSON, wantWidth: 1024, wantHeight: 768, wantFormat: "base64"},
		{name: "url", size: "512x512", format: core.ImageResponseFormatURL, wantWidth: 512, wantHeight: 512, wantFormat: "url"},
		{name: "auto size is the provider default", size: "auto"},
		{name: "nothing set"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]json.RawMessage
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request body: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"img-1","model":"black-forest-labs/FLUX.1-schnell","object":"list","data":[{"index":0,"type":"b64_json","b64_json":"aGk="}]}`))
			}))
			defer srv.Close()

			p, _ := New("test-key", srv.URL)
			resp, err := p.GenerateImage(context.Background(), core.ImageRequest{
				Model:          "black-forest-labs/FLUX.1-schnell",
				Prompt:         "a red apple",
				Size:           tc.size,
				ResponseFormat: tc.format,
			})
			if err != nil {
				t.Fatalf("GenerateImage() error: %v", err)
			}
			if len(resp.Data) != 1 || resp.Data[0].B64JSON != "aGk=" {
				t.Errorf("Data = %+v, want the base64 image", resp.Data)
			}

			if raw, ok := body["size"]; ok {
				t.Errorf("size = %s forwarded, want width and height instead", raw)
			}
			assertIntField(t, body, "width", tc.wantWidth)
			assertIntField(t, body, "height", tc.wantHeight)
			var gotFormat string
			if raw, ok := body["response_format"]; ok {
				_ = json.Unmarshal(raw, &gotFormat)
			}
			if gotFormat != tc.wantFormat {
				t.Errorf("response_format = %q, want %q", gotFormat, tc.wantFormat)
			}
		})
	}
}

// TestTogetherProvider_GenerateImage_RefusesMalformedSize pins that a size no
// width and height can be read from is the caller's 400, decided before any
// upstream call, rather than an image of whatever size the model defaults to.
func TestTogetherProvider_GenerateImage_RefusesMalformedSize(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	p, _ := New("test-key", srv.URL)
	_, err := p.GenerateImage(context.Background(), core.ImageRequest{
		Model:  "black-forest-labs/FLUX.1-schnell",
		Prompt: "a red apple",
		Size:   "large",
	})
	if got := core.ParseStatusCode(err); got != http.StatusBadRequest {
		t.Fatalf("ParseStatusCode(%v) = %d, want 400", err, got)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("upstream calls = %d, want 0", got)
	}
}

func assertIntField(t *testing.T, body map[string]json.RawMessage, key string, want int) {
	t.Helper()
	raw, ok := body[key]
	if want == 0 {
		if ok {
			t.Errorf("%s = %s forwarded, want it omitted", key, raw)
		}
		return
	}
	var got int
	if !ok || json.Unmarshal(raw, &got) != nil || got != want {
		t.Errorf("%s = %s, want %d", key, raw, want)
	}
}
