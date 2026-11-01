package handler

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/providers/openai"
)

// audioUploadWithValues builds a whisper-1 upload carrying fields as form
// values, one part per value.
func audioUploadWithValues(t *testing.T, path string, fields map[string][]string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", "speech.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("RIFF....WAVE"))
	_ = form.WriteField("model", "whisper-1")
	for name, values := range fields {
		for _, value := range values {
			_ = form.WriteField(name, value)
		}
	}
	_ = form.Close()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	return r
}

// verbose_json and json are served with every member the upstream returned —
// words and logprobs among them — but the upstream returns word timestamps only
// when asked with timestamp_granularities[]=word, and token logprobs only with
// include[]=logprobs. Both are array fields, which the OpenAI SDKs send as one
// "name[]" part per value, and the handler read neither: the upstream was never
// asked, and a caller that asked got segments alone, or no logprobs, under a
// 200 with nothing to say the request had been changed.
func TestTranscriptions_ForwardsTimestampGranularitiesAndInclude(t *testing.T) {
	var (
		mu                           sync.Mutex
		gotGranularities, gotInclude []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("upstream ParseMultipartForm: %v", err)
		}
		mu.Lock()
		gotGranularities = r.MultipartForm.Value["timestamp_granularities[]"]
		gotInclude = r.MultipartForm.Value["include[]"]
		mu.Unlock()
		body := map[string]any{"task": "transcribe", "language": "english", "duration": 1.5, "text": "hello there",
			"segments": []any{map[string]any{"id": 0, "start": 0, "end": 1.5, "text": "hello there"}}}
		if slices.Contains(gotGranularities, "word") {
			body["words"] = []any{map[string]any{"word": "hello", "start": 0, "end": 0.5}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer upstream.Close()

	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "openai", Models: []string{"whisper-1"}}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p, err := openai.New("test-key", upstream.URL)
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}
	gw.RegisterProvider(p)

	for _, tc := range []struct {
		name  string
		field string
	}{
		{name: "bracketed, as the OpenAI SDKs send them", field: "timestamp_granularities[]"},
		{name: "unbracketed", field: "timestamp_granularities"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			gotGranularities, gotInclude = nil, nil
			mu.Unlock()
			req := audioUploadWithValues(t, "/v1/audio/transcriptions", map[string][]string{
				"response_format": {"verbose_json"},
				tc.field:          {"word", "segment"},
				"include[]":       {"logprobs"},
			})
			w := httptest.NewRecorder()
			Transcriptions(gw, false)(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(gotGranularities, []string{"word", "segment"}) {
				t.Errorf("upstream timestamp_granularities[] = %q, want [word segment]", gotGranularities)
			}
			if !slices.Equal(gotInclude, []string{"logprobs"}) {
				t.Errorf("upstream include[] = %q, want [logprobs]", gotInclude)
			}
			var served map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &served); err != nil {
				t.Fatalf("decode: %v: %s", err, w.Body.String())
			}
			if _, ok := served["words"]; !ok {
				t.Errorf("served %s, want the word timestamps the caller asked for", w.Body.String())
			}
		})
	}
}
