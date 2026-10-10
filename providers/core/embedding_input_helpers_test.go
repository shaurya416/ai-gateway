package core

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// TestNormalizeEmbeddingInput is the table-driven contract for the polymorphic
// Input: bare string and []string keep their wire form, []any of strings is
// coerced to []string, and empty arrays / nil / unsupported types are rejected.
func TestNormalizeEmbeddingInput(t *testing.T) {
	tests := []struct {
		name    string
		input   any
		want    any
		wantErr bool
	}{
		{name: "bare string preserved", input: "hello", want: "hello"},
		{name: "empty string is valid", input: "", want: ""},
		{name: "string slice preserved", input: []string{"a", "b"}, want: []string{"a", "b"}},
		{name: "any slice of strings coerced", input: []any{"a", "b"}, want: []string{"a", "b"}},
		{name: "empty string slice rejected", input: []string{}, wantErr: true},
		{name: "empty any slice rejected", input: []any{}, wantErr: true},
		{name: "any slice with non-string rejected", input: []any{"a", 1}, wantErr: true},
		{name: "nil rejected", input: nil, wantErr: true},
		{name: "unsupported type rejected", input: 42, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeEmbeddingInput(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (result=%#v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestValidateEmbeddingEncodingFormat(t *testing.T) {
	for _, ok := range []string{"", "float"} {
		if err := ValidateEmbeddingEncodingFormat(ok); err != nil {
			t.Errorf("format %q: unexpected error %v", ok, err)
		}
	}
	for _, bad := range []string{"base64", "int8", "FLOAT"} {
		if err := ValidateEmbeddingEncodingFormat(bad); err == nil {
			t.Errorf("format %q: expected error, got nil", bad)
		}
	}
}

// TestValidateEmbeddingEncodingFormat_Rejects400 pins the classification of the
// rejection, not just its existence. A bare error carries no status: it reaches
// the caller as a 500 saying the gateway is broken, and shouldRetry reads a
// status-less error as a transport failure and spends the whole retry budget on
// a value that can never become valid. All fifteen callers of this helper
// inherit the answer from here.
func TestValidateEmbeddingEncodingFormat_Rejects400(t *testing.T) {
	for _, bad := range []string{"base64", "int8", "FLOAT"} {
		t.Run(bad, func(t *testing.T) {
			err := ValidateEmbeddingEncodingFormat(bad)
			if err == nil {
				t.Fatalf("format %q: expected error, got nil", bad)
			}
			if got := ParseStatusCode(err); got != http.StatusBadRequest {
				t.Errorf("ParseStatusCode(%v) = %d, want %d", err, got, http.StatusBadRequest)
			}
			var statusErr *HTTPStatusError
			if !errors.As(err, &statusErr) {
				t.Fatalf("error is %T, want *HTTPStatusError", err)
			}
			// The rejected VALUE is the only actionable detail the caller gets.
			if !strings.Contains(statusErr.Message, bad) {
				t.Errorf("caller-facing message = %q, want it to name %q", statusErr.Message, bad)
			}
			// No upstream produced this, so no provider is named at it.
			if statusErr.Provider != "" {
				t.Errorf("Provider = %q, want empty for a gateway-side refusal", statusErr.Provider)
			}
		})
	}
}

// TestEmbeddingInputRefusalsAre400 pins the classification of every refusal the
// shared input validators make. Each is the caller's to fix, and none reaches an
// upstream: as bare errors they answered 500, were retried as transport
// failures, and were offered to every other target in the pool. Token ids get
// a message naming them, since that is the shape a client most often sends to a
// text-only target (LangChain's OpenAIEmbeddings does by default).
func TestEmbeddingInputRefusalsAre400(t *testing.T) {
	validators := map[string]func(any) error{
		"NormalizeEmbeddingInput": func(in any) error { _, err := NormalizeEmbeddingInput(in); return err },
		"CoerceEmbeddingInput":    func(in any) error { _, err := CoerceEmbeddingInput(in); return err },
	}
	inputs := map[string]any{
		"nil":                nil,
		"empty string slice": []string{},
		"empty any slice":    []any{},
		"unsupported type":   42,
		"non-string element": []any{"a", 1},
		"token ids":          []any{float64(9906), float64(1917)},
		"token arrays":       []any{[]any{float64(9906)}},
	}
	for vname, validate := range validators {
		for iname, input := range inputs {
			t.Run(vname+"/"+iname, func(t *testing.T) {
				err := validate(input)
				if got := ParseStatusCode(err); got != http.StatusBadRequest {
					t.Fatalf("error = %v (status %d), want a 400", err, got)
				}
				var statusErr *HTTPStatusError
				if !errors.As(err, &statusErr) {
					t.Fatalf("error is %T, want *HTTPStatusError", err)
				}
				if statusErr.Provider != "" {
					t.Errorf("Provider = %q, want empty for a gateway-side refusal", statusErr.Provider)
				}
				if strings.HasPrefix(iname, "token") && !strings.Contains(statusErr.Message, "token ids") {
					t.Errorf("message = %q, want it to name token-id input", statusErr.Message)
				}
			})
		}
	}
}

// TestNormalizeEmbeddingTokenInput is the contract for the OpenAI input union:
// text behaves as NormalizeEmbeddingInput, and token-id input decoded from JSON
// becomes the integer shapes the union carries.
func TestNormalizeEmbeddingTokenInput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input any
		want  any
		count int
	}{
		{name: "string", input: "hi", want: "hi", count: 1},
		{name: "strings", input: []any{"a", "b"}, want: []string{"a", "b"}, count: 2},
		{name: "token array", input: []any{float64(1), float64(2)}, want: []int64{1, 2}, count: 1},
		{name: "token arrays", input: []any{[]any{float64(1)}, []any{float64(2), float64(3)}}, want: [][]int64{{1}, {2, 3}}, count: 2},
		{name: "typed token arrays", input: [][]int64{{7}}, want: [][]int64{{7}}, count: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeEmbeddingTokenInput(tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
			if n := EmbeddingInputCount(got); n != tc.count {
				t.Errorf("EmbeddingInputCount = %d, want %d", n, tc.count)
			}
		})
	}
	for name, input := range map[string]any{
		"empty":             []any{},
		"fraction":          []any{float64(0.5)},
		"negative":          []any{float64(-3)},
		"too large":         []any{float64(1 << 60)},
		"mixed":             []any{float64(1), "x"},
		"empty inner array": []any{[]any{}},
		"empty typed inner": [][]int64{{}},
		"ragged nesting":    []any{[]any{float64(1)}, float64(2)},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			_, err := NormalizeEmbeddingTokenInput(input)
			if got := ParseStatusCode(err); got != http.StatusBadRequest {
				t.Errorf("error = %v (status %d), want a 400", err, got)
			}
		})
	}
}
