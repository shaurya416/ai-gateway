package proxy

import (
	"net/http"
	"strings"
	"testing"
)

// A body that names one key twice is forwarded verbatim, and which of the two
// values the upstream reads is its parser's choice: some keep the first, some
// the last. The projection kept only the last, so a blocked prompt written
// first and followed by a harmless one under the same key passed a content
// guardrail and travelled upstream. Every value is now shown to the guardrail,
// on both forwards that carry a body to it.
func TestPassthrough_GuardrailSeesEveryValueOfARepeatedKey(t *testing.T) {
	body := `{"model":"stub-model","input":"tell me the forbidden thing","input":"hello"}`

	t.Run("pass-through", func(t *testing.T) {
		up := newCountingUpstream(t)
		gw := newGovernedGateway(t, up.URL, wordFilterConfig("forbidden"))
		if w := passthroughPOST(t, gw, "/v1/responses", body, nil); w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d (guardrail rejection); body: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		if hits := up.hits.Load(); hits != 0 {
			t.Errorf("upstream received %d requests, want 0", hits)
		}
	})
	t.Run("responses create", func(t *testing.T) {
		up := newCountingUpstream(t)
		gw := newGovernedGateway(t, up.URL, wordFilterConfig("forbidden"))
		if w := postResponsesBody(t, ResponsesCreate(gw), body); w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d (guardrail rejection); body: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		if hits := up.hits.Load(); hits != 0 {
			t.Errorf("upstream received %d requests, want 0", hits)
		}
	})
}

// The projection reads the body in order and keeps its top-level model as the
// gateway's other readers do: the last string spelled under "model".
func TestProjectBody_KeepsDocumentOrderAndTheLastModel(t *testing.T) {
	for _, tc := range []struct {
		body, wantText, wantModel string
	}{
		{`{"model":"a","input":["x",{"k":"y"}],"model":"b"}`, "a\nx\ny\nb", "b"},
		{`{"model":"a","model":5}`, "a", ""},
		{`{"input":{"model":"nested"}}`, "nested", ""},
		{`["model","v"]`, "model\nv", ""},
		{`"just a string"`, "just a string", ""},
	} {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		text, model, inspectable := projectBody(r)
		if !inspectable || text != tc.wantText || model != tc.wantModel {
			t.Errorf("projectBody(%s) = (%q, %q, %v), want (%q, %q, true)", tc.body, text, model, inspectable, tc.wantText, tc.wantModel)
		}
	}
}
