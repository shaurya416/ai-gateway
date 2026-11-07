package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/config"
)

// The Responses id sub-routes are held to max_request_bytes like every other
// data-plane route but the file, batch and audio uploads. The route group sits
// outside the shared body-limit middleware because create applies the cap
// itself, and the id sub-routes in it applied none: a body of any size was
// streamed to responses_target under the operator's credential.
func TestResponsesIDs_BodyPastMaxRequestBytesIsRefused(t *testing.T) {
	const limit = 64

	var received atomic.Int64
	var completed atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		received.Store(n)
		if err != nil {
			return
		}
		completed.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"cancelled"}`)
	}))
	t.Cleanup(up.Close)

	t.Setenv("FERRO_MODEL_CATALOG_TIMEOUT", "0")
	gw, err := aigateway.New(config.Config{
		Strategy:        config.StrategyConfig{Mode: config.ModeSingle},
		Targets:         []config.Target{{VirtualKey: "openai"}},
		ResponsesTarget: "openai",
		MaxRequestBytes: limit,
	})
	if err != nil {
		t.Fatalf("aigateway.New: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })
	gw.RegisterProvider(&proxiableStub{name: "openai", baseURL: up.URL})

	for _, tc := range []struct {
		name   string
		length int64 // the request's ContentLength; -1 sends it chunked
	}{
		{"content-length", 4096},
		{"chunked", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			received.Store(0)
			completed.Store(false)
			body := strings.Repeat("x", 4096)
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses/resp_1/cancel", strings.NewReader(body))
			r.ContentLength = tc.length
			w := httptest.NewRecorder()
			ResponsesIDs(gw)(w, r)

			if w.Code != http.StatusRequestEntityTooLarge {
				t.Errorf("status = %d, want 413; body: %s", w.Code, w.Body.String())
			}
			if completed.Load() {
				t.Errorf("responses_target read the whole %d-byte body (%d bytes) and answered it; max_request_bytes is %d", len(body), received.Load(), limit)
			}
		})
	}

	// A body inside the cap is forwarded as before.
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses/resp_1/cancel", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	ResponsesIDs(gw)(w, r)
	if w.Code != http.StatusOK || !completed.Load() {
		t.Errorf("a body inside max_request_bytes: status = %d, forwarded = %v; body: %s", w.Code, completed.Load(), w.Body.String())
	}
}
