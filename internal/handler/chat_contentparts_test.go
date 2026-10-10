package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/providers/anthropic"
)

// TestChatCompletions_UncarriableContentPartIsRefused pins what the chat
// surface does with a content part it cannot carry. A part is decoded into a
// type that holds text and image_url only, so the payload of any other part —
// a file, an input_audio clip — was discarded at the edge and the request went
// on without it: the adapters that translate parts skip a type they do not
// know, so the model answered 200 as though nothing had been attached, and the
// OpenAI-wire adapters forwarded an empty {"type":"file"} for the upstream to
// refuse as a field the caller had sent. The caller is now told, before any
// target is called. An image_url part with no URL carries nothing either, and
// is refused the same way.
func TestChatCompletions_UncarriableContentPartIsRefused(t *testing.T) {
	const model = "claude-sonnet-4-5"

	var (
		mu     sync.Mutex
		bodies []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	provider, err := anthropic.New("test-key", upstream.URL)
	if err != nil {
		t.Fatalf("anthropic.New: %v", err)
	}
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: anthropic.Name, Models: []string{model}}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw.RegisterProvider(provider)

	chat := func(parts string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		body := `{"model":"` + model + `","messages":[{"role":"user","content":[{"type":"text","text":"summarise the attachment"},` + parts + `]}]}`
		ChatCompletions(gw)(w, jsonRequest(t, "/v1/chat/completions", body))
		return w
	}
	calls := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(bodies)
	}

	for name, part := range map[string]string{
		"file":                        `{"type":"file","file":{"filename":"q3.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQK"}}`,
		"input_audio":                 `{"type":"input_audio","input_audio":{"data":"UklGRg==","format":"wav"}}`,
		"unknown type":                `{"type":"video_url","video_url":{"url":"https://example.com/v.mp4"}}`,
		"no type":                     `{"text":"untyped"}`,
		"image_url without a url":     `{"type":"image_url"}`,
		"image_url with an empty url": `{"type":"image_url","image_url":{"url":""}}`,
	} {
		w := chat(part)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s part: status = %d, want 400: %s", name, w.Code, w.Body.String())
			continue
		}
		if !strings.Contains(w.Body.String(), `"type":"invalid_request_error"`) || !strings.Contains(w.Body.String(), "messages[0]: content[1]") {
			t.Errorf("%s part: body = %s, want an invalid_request_error naming messages[0]: content[1]", name, w.Body.String())
		}
	}
	if got := calls(); got != 0 {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("a request carrying an uncarriable part reached the upstream (%d call(s)) without it: %v", got, bodies)
	}

	// The two part types the gateway carries still route.
	if w := chat(`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}`); w.Code != http.StatusOK {
		t.Fatalf("text + image_url: status = %d, want 200: %s", w.Code, w.Body.String())
	}
}
