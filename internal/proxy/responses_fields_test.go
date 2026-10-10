package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The governed fields of a Responses body are read the way the upstream reads
// them, and a body whose spelling leaves that in doubt is not read at all.
// encoding/json matches a struct field to a key spelled in any case, so a body
// carrying "max_output_tokens" and then "Max_Output_Tokens" had the second,
// lower value approved by the max-token guardrail while an upstream matching
// keys exactly generated against the first. The same folding let a "MODEL"
// key choose the routing and pricing model while that upstream ran the
// "model" one.
func TestPeekResponsesFields_ReadsExactKeys(t *testing.T) {
	cases := map[string]struct {
		body          string
		wantModel     string
		wantMax       int
		wantAmbiguous bool
	}{
		"plain":                {body: `{"model":"m","max_output_tokens":64}`, wantModel: "m", wantMax: 64},
		"duplicate keeps last": {body: `{"model":"a","model":"m","max_output_tokens":1,"max_output_tokens":64}`, wantModel: "m", wantMax: 64},
		"folded model after":   {body: `{"model":"m","MODEL":"other"}`, wantAmbiguous: true},
		"folded model before":  {body: `{"Model":"other","model":"m"}`, wantAmbiguous: true},
		"folded ceiling after": {body: `{"model":"m","max_output_tokens":100000,"Max_Output_Tokens":10}`, wantAmbiguous: true},
		"folded ceiling only":  {body: `{"model":"m","MAX_OUTPUT_TOKENS":100000}`, wantAmbiguous: true},
		"unicode fold":         {body: "{\"model\":\"m\",\"max_output_to\u212aens\":100000}", wantAmbiguous: true},
		"unrelated keys":       {body: `{"model":"m","modelx":"y","Input":"hi"}`, wantModel: "m"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(tc.body))
			model, maxOut, ambiguous := peekResponsesFields(r)
			if model != tc.wantModel || maxOut != tc.wantMax || ambiguous != tc.wantAmbiguous {
				t.Errorf("peekResponsesFields(%s) = (%q, %d, %v), want (%q, %d, %v)",
					tc.body, model, maxOut, ambiguous, tc.wantModel, tc.wantMax, tc.wantAmbiguous)
			}
		})
	}
}

// End to end: a folded-case duplicate cannot slip a ceiling past a max-token
// guardrail, and an ambiguous body is refused before anything is forwarded.
func TestResponsesCreate_RefusesAmbiguousGovernedFields(t *testing.T) {
	up := newCountingUpstream(t)
	gw := newGovernedGateway(t, up.URL, maxTokenConfig(0))
	h := ResponsesCreate(gw)

	for _, body := range []string{
		`{"model":"stub-model","input":"hi","max_output_tokens":100000,"Max_Output_Tokens":10}`,
		`{"model":"stub-model","input":"hi","MAX_OUTPUT_TOKENS":100000}`,
		`{"model":"stub-model","input":"hi","Model":"other-model"}`,
	} {
		if w := postResponsesBody(t, h, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body: %s", body, w.Code, w.Body.String())
		}
	}
	if hits := up.hits.Load(); hits != 0 {
		t.Errorf("upstream received %d requests, want 0 — an ambiguous body must not be forwarded", hits)
	}

	if w := postResponsesBody(t, h, `{"model":"stub-model","input":"hi","max_output_tokens":64}`); w.Code != http.StatusOK {
		t.Errorf("exactly spelled body: status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
}

func postResponsesBody(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h(w, r)
	return w
}
