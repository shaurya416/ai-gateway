package otel

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// sprint formats v via fmt.Sprintf("%v", v). Centralised so callers
// don't pull fmt directly into hot files.
func sprint(v any) string { return fmt.Sprintf("%v", v) }

// validUTF8 returns s unchanged when it is valid UTF-8, and otherwise with each
// invalid byte sequence replaced by U+FFFD.
//
// Every string the provider takes from a caller and puts on a span passes
// through it, as does every error message a span records. OTLP carries
// attribute keys and values, status descriptions and event attributes as
// protobuf strings, which must be UTF-8, and the SDK does not check: one
// request value carrying raw bytes — a multipart form field, or an error
// message quoting one — failed to marshal and took every span exported in the
// same batch down with it. The check is allocation-free for valid input, which
// is all of it in practice.
func validUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

// validUTF8Slice applies validUTF8 to each element, copying v only when an
// element has to change so the caller's slice is never written to.
func validUTF8Slice(v []string) []string {
	for i, s := range v {
		if utf8.ValidString(s) {
			continue
		}
		out := slices.Clone(v)
		for j := i; j < len(out); j++ {
			out[j] = validUTF8(out[j])
		}
		return out
	}
	return v
}
