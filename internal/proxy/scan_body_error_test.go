package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
)

// The first read of a forwarded body is the search for its model, and a body
// that failed there — cut off past the request-body limit, or with framing that
// broke mid-upload — ended that search the way a body naming no model does. The
// /v1/* pass-through answered 400 asking the caller to "include a \"model\"
// field", and /v1/responses answered "model is required", to callers whose
// bodies carried one; the routed surfaces, and these forwards once the body is
// streaming, answer the same failure 413 or 400 invalid request body.
func TestForwards_BodyFailureBeforeTheModelIsTheCallers(t *testing.T) {
	up := newCountingUpstream(t)
	gw, _ := abortTestGateway(t, up.URL)

	cases := map[string]struct {
		h        http.HandlerFunc
		path     string
		oversize bool
		want     int
		wantCode string
	}{
		"passthrough past the size limit": {h: Handler(gw), path: "/v1/fine_tuning/jobs", oversize: true, want: http.StatusRequestEntityTooLarge, wantCode: "request_too_large"},
		"passthrough broken framing":      {h: Handler(gw), path: "/v1/fine_tuning/jobs", want: http.StatusBadRequest, wantCode: "invalid_request"},
		"responses broken framing":        {h: ResponsesCreate(gw), path: "/v1/responses", want: http.StatusBadRequest, wantCode: "invalid_request"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// The model follows a large field, so the scan fails before it.
			prefix := `{"input":"` + strings.Repeat("x", 4<<10)
			body := io.MultiReader(strings.NewReader(prefix), iotest.ErrReader(io.ErrUnexpectedEOF))
			if tc.oversize {
				body = strings.NewReader(prefix + `","model":"stub-model"}`)
			}
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.path, body)
			r.Header.Set("Content-Type", "application/json")
			r.ContentLength = -1
			w := httptest.NewRecorder()
			if tc.oversize {
				r.Body = http.MaxBytesReader(w, r.Body, 2<<10)
			}
			tc.h(w, r)

			var got struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &got)
			if w.Code != tc.want || got.Error.Code != tc.wantCode {
				t.Errorf("status = %d code %q, want %d %q; body: %s", w.Code, got.Error.Code, tc.want, tc.wantCode, w.Body.String())
			}
			if strings.Contains(got.Error.Message, "model") {
				t.Errorf("answer asks for the model the body carried: %q", got.Error.Message)
			}
			if n := up.hits.Load(); n != 0 {
				t.Errorf("upstream received %d requests, want 0", n)
			}
		})
	}
}
