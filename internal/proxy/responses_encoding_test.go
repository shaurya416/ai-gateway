package proxy

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/providers"
)

// usageRecordingSource is the real gateway with the one call ResponsesCreate
// prices through wrapped, so a test can read the usage the forward captured.
type usageRecordingSource struct {
	*aigateway.Gateway
	captured providers.Usage
}

func (s *usageRecordingSource) RouteResponsesWithPricingProvider(ctx context.Context, target, priceProvider, model, body string, bodyInspectable bool, maxOutputTokens int, usage *providers.Usage, forward func(context.Context) error) error {
	err := s.Gateway.RouteResponsesWithPricingProvider(ctx, target, priceProvider, model, body, bodyInspectable, maxOutputTokens, usage, forward)
	s.captured = *usage
	return err
}

// TestResponsesCreate_UsageCapturedWhenClientAcceptsGzip pins that a caller
// advertising compression is still priced. The upstream compresses whenever it
// is asked to, as a real API does; the client sends the Accept-Encoding the
// common SDK HTTP stacks send by default. Forwarded as-is, that header kept the
// transport from decoding the response, the usage tee read gzip bytes, and the
// request was recorded unpriced while the caller got a normal 200.
func TestResponsesCreate_UsageCapturedWhenClientAcceptsGzip(t *testing.T) {
	const jsonBody = `{"id":"resp_1","object":"response","status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	const sseBody = "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}` + "\n\n"

	cases := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "json", contentType: "application/json", body: jsonBody},
		{name: "event stream", contentType: "text/event-stream", body: sseBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
					_, _ = io.WriteString(w, tc.body)
					return
				}
				w.Header().Set("Content-Encoding", "gzip")
				zw := gzip.NewWriter(w)
				_, _ = io.WriteString(zw, tc.body)
				_ = zw.Close()
			}))
			t.Cleanup(upstream.Close)

			src := &usageRecordingSource{Gateway: newGovernedGateway(t, upstream.URL+"/v1")}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses",
				strings.NewReader(`{"model":"stub-model","input":"hello"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept-Encoding", "gzip, deflate")
			rec := httptest.NewRecorder()

			ResponsesCreate(src).ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if got := src.captured; got.PromptTokens != 10 || got.CompletionTokens != 5 || got.TotalTokens != 15 {
				t.Errorf("captured usage = %+v, want 10 in / 5 out / 15 total — a caller accepting gzip must still be priced", got)
			}
			// Whatever encoding reaches the caller, the body it decodes to must
			// be the upstream's own.
			got := rec.Body.String()
			if rec.Header().Get("Content-Encoding") == "gzip" {
				zr, err := gzip.NewReader(strings.NewReader(got))
				if err != nil {
					t.Fatalf("gzip reader: %v", err)
				}
				plain, _ := io.ReadAll(zr)
				got = string(plain)
			}
			if got != tc.body {
				t.Errorf("body = %q, want the upstream body %q", got, tc.body)
			}
		})
	}
}
