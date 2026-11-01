package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"unicode/utf8"
)

// HeaderRoutingMetadata is the request header conditional routing reads
// (key: metadata, field: <entry>). It never reaches a provider.
const HeaderRoutingMetadata = "X-Gateway-Metadata"

// Limits the gateway enforces on HeaderRoutingMetadata. A header over either
// is the caller's 400.
const (
	MaxRoutingMetadataEntries = 32
	MaxRoutingMetadataBytes   = 4096
)

// ErrInvalidRoutingMetadata is wrapped by every error EncodeRoutingMetadata
// returns.
var ErrInvalidRoutingMetadata = errors.New("invalid routing metadata")

// EncodeRoutingMetadata returns the X-Gateway-Metadata header value for md,
// refusing what the gateway would refuse so the mistake is found before a
// request is made rather than as a 400.
//
// Every value must be a string, a bool, or a finite number of any Go numeric
// type (json.Number included); a nested object, an array or a nil is refused.
// A type defined on one of those — time.Duration, a caller's own string enum —
// is accepted when it encodes as a JSON string, number or boolean.
// At most MaxRoutingMetadataEntries entries are allowed, and the encoded value
// must fit in MaxRoutingMetadataBytes. A rule matches a value as a string, so
// 42 and "42" select the same rule.
//
// The value is a JSON object with its keys sorted, so the same map always
// produces the same header. Characters outside printable ASCII are written as
// \u escapes: the header stays valid for every HTTP hop, and the gateway's JSON
// decoder reads the escapes back as the original text. The byte limit applies
// to that escaped form, because it is what the gateway measures.
func EncodeRoutingMetadata(md map[string]any) (string, error) {
	if len(md) > MaxRoutingMetadataEntries {
		return "", fmt.Errorf("%w: %d entries, at most %d are allowed", ErrInvalidRoutingMetadata, len(md), MaxRoutingMetadataEntries)
	}
	for key, value := range md {
		if err := checkMetadataValue(value); err != nil {
			return "", fmt.Errorf("%w: entry %q %s", ErrInvalidRoutingMetadata, key, err.Error())
		}
		if !utf8.ValidString(key) {
			return "", fmt.Errorf("%w: entry key %q is not valid UTF-8", ErrInvalidRoutingMetadata, key)
		}
	}
	if md == nil {
		md = map[string]any{}
	}
	raw, err := json.Marshal(md)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidRoutingMetadata, err)
	}
	encoded := asciiJSON(raw)
	if len(encoded) > MaxRoutingMetadataBytes {
		return "", fmt.Errorf("%w: encoded value is %d bytes, at most %d are allowed", ErrInvalidRoutingMetadata, len(encoded), MaxRoutingMetadataBytes)
	}
	return encoded, nil
}

// checkMetadataValue accepts the scalar kinds the gateway accepts: a JSON
// string, number or boolean.
//
// The kind is read, not the type, so a type defined on one of those kinds — a
// caller's own enum, time.Duration — is accepted as the value it encodes as.
// Such a type may carry its own MarshalJSON or MarshalText, so its encoding is
// checked as well: that, not the Go type, is what the gateway judges.
func checkMetadataValue(value any) error {
	switch v := value.(type) {
	case json.Number:
		s := v.String()
		if s == "" || (s[0] != '-' && (s[0] < '0' || s[0] > '9')) || !json.Valid([]byte(s)) {
			return fmt.Errorf("is not a JSON number: %q", s)
		}
		return nil
	case nil:
		return errors.New("is null; values must be a string, number or boolean")
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.String:
		if !utf8.ValidString(rv.String()) {
			return errors.New("is not valid UTF-8")
		}
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
	case reflect.Float32, reflect.Float64:
		if err := checkFinite(rv.Float()); err != nil {
			return err
		}
	default:
		return fmt.Errorf("is a %T; values must be a string, number or boolean", value)
	}
	if rv.Type().PkgPath() == "" {
		// A predeclared type, whose encoding is the scalar itself.
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("cannot be encoded: %w", err)
	}
	if !isJSONScalar(raw) {
		return fmt.Errorf("is a %T, which encodes as %s; values must be a string, number or boolean", value, raw)
	}
	return nil
}

// isJSONScalar reports whether raw, a value json.Marshal produced, is a JSON
// string, number or boolean.
func isJSONScalar(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	switch c := raw[0]; c {
	case '"', 't', 'f', '-':
		return true
	default:
		return c >= '0' && c <= '9'
	}
}

func checkFinite(f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return errors.New("is not a finite number")
	}
	return nil
}

// asciiJSON rewrites every byte outside printable ASCII in a JSON document as
// a \u escape. Such bytes only occur inside JSON strings, where the escape is
// an exact equivalent.
func asciiJSON(raw []byte) string {
	clean := true
	for _, b := range raw {
		if b < 0x20 || b >= 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return string(raw)
	}
	var out bytes.Buffer
	out.Grow(len(raw) + 16)
	for len(raw) > 0 {
		r, size := utf8.DecodeRune(raw)
		raw = raw[size:]
		if r >= 0x20 && r < 0x7f {
			out.WriteByte(byte(r))
			continue
		}
		if r > 0xFFFF {
			r -= 0x10000
			fmt.Fprintf(&out, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
			continue
		}
		fmt.Fprintf(&out, `\u%04x`, r)
	}
	return out.String()
}
