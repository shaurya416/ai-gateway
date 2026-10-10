package proxy

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/config"
)

// compressingUpstream answers every request with body, gzip-encoded whenever
// the request's Accept-Encoding allows it — what any HTTP server with response
// compression enabled does.
func compressingUpstream(t *testing.T, contentType, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			_, _ = io.WriteString(w, body)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = io.WriteString(zw, body)
		_ = zw.Close()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A client that accepts a compressed response — httpx, which the official
// OpenAI Python SDK uses, sends "Accept-Encoding: gzip, deflate" on every
// request — had that header forwarded upstream. The upstream compressed its
// answer, the usage tee parsed gzip bytes, found no usage, and the request was
// recorded unpriced: no tokens, no cost, nothing for a budget to charge.
func TestResponsesCreate_UsageIsCapturedFromCompressedResponse(t *testing.T) {
	cases := map[string]struct {
		contentType, body string
	}{
		"json": {
			contentType: "application/json",
			body:        `{"id":"resp_1","object":"response","status":"completed","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150}}`,
		},
		"stream": {
			contentType: "text/event-stream",
			body: "event: response.created\n" +
				`data: {"type":"response.created","response":{"id":"resp_1","usage":null}}` + "\n\n" +
				"event: response.completed\n" +
				`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150}}}` + "\n\n",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			up := compressingUpstream(t, tc.contentType, tc.body)

			t.Setenv("FERRO_MODEL_CATALOG_TIMEOUT", "0")
			store := &recordingLogStore{}
			gw, err := aigateway.New(config.Config{
				Strategy: config.StrategyConfig{Mode: config.ModeSingle},
				Targets:  []config.Target{{VirtualKey: "stub", Models: []string{"stub-model"}}},
				Plugins:  requestLoggerConfig(),
			})
			if err != nil {
				t.Fatalf("aigateway.New: %v", err)
			}
			t.Cleanup(func() { _ = gw.Close() })
			gw.RegisterProvider(&proxiableStub{name: "stub", baseURL: up.URL, models: []string{"stub-model"}})
			gw.SetRequestLogWriter(store)
			if err := gw.LoadPlugins(); err != nil {
				t.Fatalf("LoadPlugins: %v", err)
			}

			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses",
				strings.NewReader(`{"model":"stub-model","input":"hi"}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Accept-Encoding", "gzip, deflate")
			w := httptest.NewRecorder()
			ResponsesCreate(gw)(w, r)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
			}
			// The client still receives the upstream's response, readable.
			got := w.Body.String()
			if w.Header().Get("Content-Encoding") == "gzip" {
				zr, err := gzip.NewReader(strings.NewReader(got))
				if err != nil {
					t.Fatalf("gzip reader: %v", err)
				}
				decoded, _ := io.ReadAll(zr)
				got = string(decoded)
			}
			if got != tc.body {
				t.Errorf("client body = %q, want the upstream's %q", got, tc.body)
			}

			var priced bool
			for _, e := range store.all() {
				if e.Stage == "after_request" && e.PromptTokens == 100 && e.CompletionTokens == 50 {
					priced = true
				}
			}
			if !priced {
				t.Errorf("no after_request row carries the response usage (100 in / 50 out); rows: %+v", store.all())
			}
		})
	}
}
