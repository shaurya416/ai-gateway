package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// projectionCap bounds how much of a request body the pass-through will buffer
// in order to project it as text for a content guardrail.
//
// It exists because this surface carries bodies the routed surfaces never see:
// a /v1/files upload or a /v1/audio/transcriptions payload can be hundreds of
// megabytes, and buffering one to search it for prose would trade a governance
// gap for an out-of-memory. Past the cap the body is reported UNINSPECTABLE
// rather than partially inspected — a guardrail shown the first megabyte of a
// body has not screened the body, and reporting a partial read as a full one is
// the same silent pass this whole path exists to close.
//
// This is the same bound Envoy's ext_authz draws, and it fails the same way:
// its with_request_body has a max_request_bytes past which the request is
// refused with a 413, explicitly overriding failure_mode_allow.
const projectionCap = 1 << 20 // 1 MiB

// projectBody returns a best-effort text projection of a JSON request body,
// together with whether that projection can be trusted to contain everything a
// content guardrail would want to read.
//
// The body is always restored, so the caller can forward it afterwards exactly
// as it arrived.
//
// What a projection covers: every STRING VALUE anywhere in a JSON body, at any
// depth, joined by newlines. That deliberately over-collects — a model id, a
// file name and a tool name land in it alongside the prose — because a
// guardrail seeing more than the prompt can only produce a false refusal, while
// a guardrail seeing less than the prompt produces a forwarded violation. Keys
// are not collected: they are the schema, not the caller's content.
//
// What it cannot cover, and so reports as uninspectable:
//
//   - a body that is not JSON at all — a multipart /v1/files upload, raw audio
//   - a body larger than projectionCap
//   - a body that is malformed JSON, or that could not be read
//
// A body that is absent or empty is inspectable and projects to nothing: a GET
// /v1/files or a DELETE carries no content, so there is nothing a guardrail was
// prevented from seeing. That is a different fact from "there is content and it
// could not be read", and only the second one has to fail closed.
//
// text-only projection. Base64 inside a JSON string is collected as
// the base64, which no blocklist will match — decoding every string that looks
// like base64 is a decoder, not a projection. Add it when a deployment actually
// needs to screen inline attachments.
//
// model is the body's top-level "model" string when the projection read one,
// empty otherwise; the decode that projects the body has already found it.
func projectBody(r *http.Request) (text, model string, inspectable bool) {
	if r.Body == nil || r.ContentLength == 0 {
		return "", "", true
	}

	// One byte past the cap, so a body sitting exactly at the limit is still
	// distinguishable from one that exceeds it.
	buf, readErr := io.ReadAll(io.LimitReader(r.Body, projectionCap+1))
	// Restore before returning down any path: the forward that follows reads
	// this body, and a refusal path may still need it intact for logging.
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), r.Body))

	if readErr != nil {
		// Includes http.MaxBytesError from the body-limit middleware. A body the
		// gateway could not finish reading is a body it did not inspect.
		return "", "", false
	}
	if len(buf) > projectionCap {
		return "", "", false
	}

	if !json.Valid(buf) {
		return "", "", false
	}
	return projectJSON(buf)
}

// projectJSON joins every non-empty string value of the valid JSON document
// doc, one per line in document order, and returns its top-level "model".
// inspectable is false only when doc could not be read to its end.
//
// The document is read token by token rather than decoded into a map, because
// a decoded map keeps only the last value of a key the body names twice. The
// body is forwarded verbatim, and which of those values the upstream reads is
// its parser's choice — some keep the first — so a prompt written first and
// followed by a harmless one under the same key reached the upstream unscreened.
// Every value is collected instead.
//
// model is the last string spelled under a top-level "model" key, the value
// encoding/json reads into a map: a later non-string spelling reads as none.
func projectJSON(doc []byte) (text, model string, inspectable bool) {
	type container struct {
		object    bool
		expectKey bool   // object only: the next token is a key
		key       string // object only: the key the next value belongs to
	}
	var (
		sb    strings.Builder
		stack []container
	)
	// valueRead records that a value has ended: the next token of an
	// enclosing object is a key again.
	valueRead := func() {
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].expectKey = true
		}
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	// A number is never decoded, so one past float64's range, which is valid
	// JSON, cannot end the walk early.
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return sb.String(), model, true
		}
		if err != nil {
			return "", "", false
		}
		topLevelModel := len(stack) == 1 && stack[0].object && !stack[0].expectKey && stack[0].key == "model"
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				if topLevelModel {
					model = ""
				}
				stack = append(stack, container{object: true, expectKey: true})
			case '[':
				if topLevelModel {
					model = ""
				}
				stack = append(stack, container{})
			default:
				stack = stack[:len(stack)-1]
				valueRead()
			}
			continue
		case string:
			if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].expectKey {
				stack[n-1].key, stack[n-1].expectKey = t, false
				continue
			}
			if topLevelModel {
				model = t
			}
			if t != "" {
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(t)
			}
		default:
			// Numbers, booleans and nulls are skipped: a blocklist is a
			// policy about prose, and no useful rule matches "3.14".
			if topLevelModel {
				model = ""
			}
		}
		valueRead()
	}
}
