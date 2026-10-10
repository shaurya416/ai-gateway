package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// JSON lets an encoder escape a forward slash as \/, and some do it by
// default — PHP's json_encode writes every "/" that way. JSON also escapes a
// character outside the Basic Multilingual Plane, most emoji among them, as a
// \u surrogate pair, which is what Python's json.dumps writes by default. The
// model scanner decoded escapes with strconv.Unquote, which accepts neither, so
// a body carrying one anywhere up to and including its model failed to parse:
// the pass-through answered 400 asking for a model field the caller had sent.
// Model ids with slashes ("meta-llama/...", "accounts/fireworks/models/..."),
// a URL in an earlier field, and an emoji in an earlier prompt were enough.
func TestExtractTopLevelModel_DecodesEveryJSONEscape(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"escaped slash in model":      {`{"model":"accounts\/fireworks\/models\/llama"}`, "accounts/fireworks/models/llama"},
		"escaped slash before model":  {`{"input":"see https:\/\/example.com","model":"gpt-4o"}`, "gpt-4o"},
		"escaped slash nested before": {`{"metadata":{"url":"https:\/\/example.com"},"model":"gpt-4o"}`, "gpt-4o"},
		"unicode escape in model":     {`{"model":"\u0067pt-4o"}`, "gpt-4o"},
		"surrogate pair before model": {`{"input":"\ud83d\ude00","model":"gpt-4o"}`, "gpt-4o"},
		"quote escape before model":   {`{"input":"say \"hi\"","model":"gpt-4o"}`, "gpt-4o"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/x", strings.NewReader(tc.body))
			model, err := ExtractTopLevelModel(r)
			if err != nil || model != tc.want {
				t.Errorf("ExtractTopLevelModel(%s) = (%q, %v), want (%q, nil)", tc.body, model, err, tc.want)
			}
			if data, _ := io.ReadAll(r.Body); string(data) != tc.body {
				t.Errorf("body after scan = %q, want it restored byte-identical", data)
			}
		})
	}
}

// The scanner skipped a nested value by recursing once per change of bracket
// type, so a body of alternating "[{" cost one call frame per byte, on the
// request's own goroutine and before anything was authorised beyond the proxy
// credential. A body at the default 10 MiB limit grew that stack past 512 MiB,
// and one slightly larger exceeded the runtime's 1 GB maximum, which is a
// fatal error that ends the whole process rather than a panic any middleware
// can recover. Skipping a value must cost the stack nothing per level, and the
// model that follows the nested value must still be found.
func TestExtractTopLevelModel_DeepNestingDoesNotGrowTheStack(t *testing.T) {
	const pairs = 1 << 20 // 2M levels of nesting in a 4 MiB body
	body := `{"input":` + strings.Repeat("[{", pairs) + strings.Repeat("}]", pairs) + `,"model":"gpt-4o"}`
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/x", strings.NewReader(body))

	var (
		before, after runtime.MemStats
		model         string
		err           error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.ReadMemStats(&before)
		model, err = ExtractTopLevelModel(r)
		// Read before this goroutine returns: its stack is still the one the
		// scan grew.
		runtime.ReadMemStats(&after)
	}()
	<-done

	if err != nil || model != "gpt-4o" {
		t.Errorf("ExtractTopLevelModel = (%q, %v), want (%q, nil)", model, err, "gpt-4o")
	}
	if after.StackInuse > before.StackInuse+8<<20 {
		t.Errorf("goroutine stacks grew %d MiB scanning a %d MiB body; skipping a nested value must not cost a call frame per level", (after.StackInuse-before.StackInuse)>>20, len(body)>>20)
	}
}

// End to end: a body that names an owned model behind an escaped slash is
// forwarded to its owner rather than refused as naming no model.
func TestPassthrough_ModelBehindEscapedSlashIsRouted(t *testing.T) {
	up := newCountingUpstream(t)
	gw := newGovernedGateway(t, up.URL)

	w := passthroughPOST(t, gw, "/v1/fine_tuning/jobs",
		`{"training_file":"https:\/\/example.com\/data.jsonl","model":"stub-model"}`, nil)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if hits := up.hits.Load(); hits != 1 {
		t.Errorf("upstream received %d requests, want 1", hits)
	}
}
