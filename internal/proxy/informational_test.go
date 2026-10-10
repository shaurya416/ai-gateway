package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/providers"
)

// A client that sends "Expect: 100-continue" — curl does for a large upload —
// had the expectation forwarded upstream, the upstream answered it with a 100,
// and the reverse proxy relayed that 100 by writing it from the response's
// header map and then clearing the map. Every header the gateway had set before
// the forward — the security headers, X-Request-ID, the CORS grant — was gone
// from the final answer, which carried only what the upstream sent.
func TestForwardsKeepGatewayHeadersAcrossInformationalResponses(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reading the body is what makes a Go server answer the expectation
		// with a 100, as any HTTP/1.1 server may.
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(up.Close)
	gw, _ := abortTestGateway(t, up.URL)
	reg := providers.NewRegistry()
	reg.Register(&proxiableStub{name: "stub", baseURL: up.URL, models: []string{"stub-model"}})

	surfaces := map[string]struct {
		h    http.HandlerFunc
		path string
	}{
		"passthrough":            {h: Handler(gw), path: "/v1/fine_tuning/jobs"},
		"passthrough-ungoverned": {h: Handler(reg), path: "/v1/fine_tuning/jobs"},
		"responses":              {h: ResponsesCreate(gw), path: "/v1/responses"},
		"files": {
			h:    BatchHandler(&batchSourceStub{target: "openai", provider: &batchStub{name: "openai", baseURL: up.URL}}),
			path: "/v1/files",
		},
	}
	for name, s := range surfaces {
		t.Run(name, func(t *testing.T) {
			// What the gateway's middleware sets on every response before the
			// handler runs.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Content-Type-Options", "nosniff")
				w.Header().Set("X-Request-ID", "req-123")
				s.h(w, r)
			}))
			t.Cleanup(srv.Close)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+s.path,
				strings.NewReader(`{"model":"stub-model","input":"hi"}`))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Provider", "stub")
			req.Header.Set("Expect", "100-continue")
			client := &http.Client{Transport: &http.Transport{ExpectContinueTimeout: 5 * time.Second}}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.Copy(io.Discard, resp.Body)

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			for header, want := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Request-ID": "req-123"} {
				if got := resp.Header.Values(header); len(got) != 1 || got[0] != want {
					t.Errorf("%s = %q, want exactly %q: the gateway's own header must survive the relayed 100", header, got, want)
				}
			}
			if got := resp.Header.Get("X-Gateway-Provider"); got == "" {
				t.Errorf("X-Gateway-Provider missing from the final response")
			}
		})
	}
}
