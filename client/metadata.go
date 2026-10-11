package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
func checkMetadataValue(value any) error {
	switch v := value.(type) {
	case string:
		if !utf8.ValidString(v) {
			return errors.New("is not valid UTF-8")
		}
		return nil
	case bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return nil
	case float32:
		return checkFinite(float64(v))
	case float64:
		return checkFinite(v)
	case json.Number:
		s := v.String()
		if s == "" || (s[0] != '-' && (s[0] < '0' || s[0] > '9')) || !json.Valid([]byte(s)) {
			return fmt.Errorf("is not a JSON number: %q", s)
		}
		return nil
	case nil:
		return errors.New("is null; values must be a string, number or boolean")
	default:
		return fmt.Errorf("is a %T; values must be a string, number or boolean", value)
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
