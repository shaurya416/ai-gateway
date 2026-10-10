package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ferro-labs/ai-gateway/internal/handler"
)

// X-Gateway-Metadata is conditional routing's request header: it addresses the
// gateway and never reaches a provider. A client that sets it once — as SDK
// default headers do — sends it on every call, and the pass-through and the
// fixed-target forwards cloned it upstream verbatim, handing the provider the
// caller's routing metadata (team, tier, tenant) along with the request.
func TestForwards_DoNotSendRoutingMetadataUpstream(t *testing.T) {
	const metadata = `{"team":"payments","tier":"gold"}`
	cases := map[string]struct {
		path  string
		serve func(upstreamURL string) http.HandlerFunc
	}{
		"passthrough": {
			path:  proxiedPath,
			serve: func(u string) http.HandlerFunc { return Handler(buildTestRegistry(t, u)) },
		},
		"fixed target": {
			path:  "/v1/files",
			serve: func(u string) http.HandlerFunc { return BatchHandler(fixedTargetSource(u)) },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			upstream, seen := upstreamCapturingTraceHeaders(t)
			req := tracedRequest(t, tc.path)
			req.Header.Set(handler.HeaderRoutingMetadata, metadata)

			w := httptest.NewRecorder()
			tc.serve(upstream.URL)(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (the request must still be forwarded); body: %s", w.Code, w.Body.String())
			}
			if got := seen.Get(handler.HeaderRoutingMetadata); got != "" {
				t.Errorf("upstream received %s %q; the routing metadata header must not reach the provider", handler.HeaderRoutingMetadata, got)
			}
		})
	}
}
