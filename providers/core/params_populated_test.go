package core

import (
	"slices"
	"testing"
)

// Every optional parameter is detected when the caller sets it and only then:
// a parameter ParamPopulated cannot see is one reject mode forwards silently
// and warn mode never names, on every provider that cannot express it.
func TestParamPopulated_DetectsEveryOptionalParameter(t *testing.T) {
	pB := func(b bool) *bool { return &b }
	set := map[string]func(*Request){
		"temperature":           func(r *Request) { r.Temperature = pF(0.5) },
		"top_p":                 func(r *Request) { r.TopP = pF(0.9) },
		"n":                     func(r *Request) { r.N = pI(2) },
		"seed":                  func(r *Request) { r.Seed = pI64(7) },
		"max_tokens":            func(r *Request) { r.MaxTokens = pI(10) },
		"max_completion_tokens": func(r *Request) { r.MaxCompletionTokens = pI(10) },
		"presence_penalty":      func(r *Request) { r.PresencePenalty = pF(0.1) },
		"frequency_penalty":     func(r *Request) { r.FrequencyPenalty = pF(0.1) },
		"stop":                  func(r *Request) { r.Stop = []string{"END"} },
		"tools":                 func(r *Request) { r.Tools = []Tool{{Type: "function", Function: Function{Name: "f"}}} },
		"tool_choice":           func(r *Request) { r.ToolChoice = "auto" },
		"parallel_tool_calls":   func(r *Request) { r.ParallelToolCalls = pB(false) },
		"response_format":       func(r *Request) { r.ResponseFormat = &ResponseFormat{Type: "json_object"} },
		"logprobs":              func(r *Request) { r.LogProbs = true },
		"top_logprobs":          func(r *Request) { r.TopLogProbs = pI(3) },
		"user":                  func(r *Request) { r.User = "u1" },
		"logit_bias":            func(r *Request) { r.LogitBias = map[string]float64{"50256": -100} },
	}
	if len(set) != len(optionalParamOrder) {
		t.Fatalf("the table covers %d parameters, optionalParamOrder lists %d", len(set), len(optionalParamOrder))
	}
	for _, name := range optionalParamOrder {
		apply, ok := set[name]
		if !ok {
			t.Fatalf("no table entry for %q", name)
		}
		t.Run(name, func(t *testing.T) {
			var req Request
			if ParamPopulated(req, name) {
				t.Fatalf("%s reported populated on an empty request", name)
			}
			apply(&req)
			if !ParamPopulated(req, name) {
				t.Fatalf("%s set by the caller is not reported populated", name)
			}
			if got := DroppedParams(req); !slices.Equal(got, []string{name}) {
				t.Fatalf("DroppedParams with nothing supported = %v, want [%s]", got, name)
			}
		})
	}
}

// An unrecognised compatibility mode fails safe: it is reported invalid and
// reads as warn, never as reject, for a caller that ignores ok.
func TestParseUnsupportedParamMode_UnknownFailsSafe(t *testing.T) {
	mode, ok := ParseUnsupportedParamMode("rejcet")
	if ok || mode != UnsupportedParamWarn {
		t.Fatalf("ParseUnsupportedParamMode(%q) = (%v, %v), want (warn, false)", "rejcet", mode, ok)
	}
}

// String round-trips every mode through its config wire name.
func TestUnsupportedParamMode_StringRoundTrips(t *testing.T) {
	for _, mode := range []UnsupportedParamMode{UnsupportedParamWarn, UnsupportedParamDrop, UnsupportedParamReject} {
		got, ok := ParseUnsupportedParamMode(mode.String())
		if !ok || got != mode {
			t.Errorf("ParseUnsupportedParamMode(%q) = (%v, %v), want (%v, true)", mode.String(), got, ok, mode)
		}
	}
}
