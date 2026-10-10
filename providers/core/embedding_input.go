package core

import (
	"fmt"
	"math"
	"net/http"
)

// EmbeddingInputError is the refusal of an embeddings "input" the caller sent
// in a shape the target cannot embed. It is a 400-carrying *HTTPStatusError for
// the reason ValidateEmbeddingEncodingFormat gives: a bare error carries no
// status, so it reached the caller as a 500 blaming the gateway, was retried as
// a transport failure, and was offered to every other target in the pool —
// none of which can change the answer.
func EmbeddingInputError(format string, args ...any) error {
	return StatusError("", http.StatusBadRequest, fmt.Sprintf(format, args...))
}

// errEmptyEmbeddingInput is the refusal of an empty embeddings "input" array.
func errEmptyEmbeddingInput() error {
	return EmbeddingInputError("embed: Input must not be an empty array")
}

// CoerceEmbeddingInput flattens an OpenAI embeddings "input" value into a plain
// []string for providers whose native API accepts only a list of texts. A bare
// string becomes a one-element slice; a []string is returned as-is; a []any is
// coerced element-by-element. An empty array, a nil input, or a non-string
// element is rejected, each with a typed 400 (see EmbeddingInputError).
func CoerceEmbeddingInput(input any) ([]string, error) {
	switch v := input.(type) {
	case string:
		return []string{v}, nil
	case []string:
		if len(v) == 0 {
			return nil, errEmptyEmbeddingInput()
		}
		return v, nil
	case []any:
		if len(v) == 0 {
			return nil, errEmptyEmbeddingInput()
		}
		return coerceAnyStrings(v)
	case nil:
		return nil, EmbeddingInputError("embed: Input must not be nil")
	default:
		return nil, EmbeddingInputError("embed: unsupported Input type %T; want string or []string", input)
	}
}

// NormalizeEmbeddingInput validates an OpenAI embeddings "input" value while
// preserving the bare-string vs array distinction that native/SDK request
// unions need (unlike CoerceEmbeddingInput, which always flattens to []string).
// A bare string is returned unchanged; a []string is returned as-is; a []any is
// coerced element-by-element to []string. An empty array, a nil input, or a
// non-string element is rejected with a typed 400. It does NOT reject empty or
// whitespace-only strings; providers that require that (azure_openai,
// databricks) keep their own stricter validator.
func NormalizeEmbeddingInput(input any) (any, error) {
	switch v := input.(type) {
	case string:
		return v, nil
	case []string:
		if len(v) == 0 {
			return nil, errEmptyEmbeddingInput()
		}
		return v, nil
	case []any:
		if len(v) == 0 {
			return nil, errEmptyEmbeddingInput()
		}
		return coerceAnyStrings(v)
	case nil:
		return nil, EmbeddingInputError("embed: Input must not be nil")
	default:
		return nil, EmbeddingInputError("embed: unsupported Input type %T; want string or []string", input)
	}
}

// NormalizeEmbeddingTokenInput is NormalizeEmbeddingInput for a target whose
// upstream takes the OpenAI embeddings input union, which carries text OR token
// ids: an array of integer token ids is one input, and an array of such arrays
// is one input per array. A token-id input decoded from JSON arrives as []any
// of float64, and is returned as []int64 or [][]int64 — the shapes openai-go's
// EmbeddingNewParamsInputUnion names — so it is forwarded as the integers it
// was sent as. Text is handled exactly as NormalizeEmbeddingInput handles it.
//
// Token ids are what LangChain's OpenAIEmbeddings sends by default (it
// tokenizes client-side to respect the context length), and the gateway serves
// them when no content guardrail is configured. A provider that does not
// forward token ids keeps NormalizeEmbeddingInput, which refuses them with a 400.
func NormalizeEmbeddingTokenInput(input any) (any, error) {
	switch v := input.(type) {
	case []int64:
		if len(v) == 0 {
			return nil, errEmptyEmbeddingInput()
		}
		return v, nil
	case [][]int64:
		if len(v) == 0 {
			return nil, errEmptyEmbeddingInput()
		}
		for i, ids := range v {
			if len(ids) == 0 {
				return nil, EmbeddingInputError("embed: Input[%d] must not be an empty token array", i)
			}
		}
		return v, nil
	case []any:
		if len(v) == 0 || !isTokenInput(v) {
			return NormalizeEmbeddingInput(input)
		}
		if _, nested := v[0].([]any); !nested {
			return tokenIDs(v, "Input")
		}
		batch := make([][]int64, len(v))
		for i, item := range v {
			ids, ok := item.([]any)
			if !ok || len(ids) == 0 {
				return nil, EmbeddingInputError("embed: Input[%d] is not a non-empty token array; an input of token arrays must hold only token arrays", i)
			}
			parsed, err := tokenIDs(ids, fmt.Sprintf("Input[%d]", i))
			if err != nil {
				return nil, err
			}
			batch[i] = parsed
		}
		return batch, nil
	default:
		return NormalizeEmbeddingInput(input)
	}
}

// isTokenInput reports whether a JSON array is shaped as token-id input — its
// first element a number or an array — rather than as text.
func isTokenInput(v []any) bool {
	switch v[0].(type) {
	case float64, []any:
		return true
	default:
		return false
	}
}

// maxTokenID is the largest integer a float64 holds exactly, so a token id
// decoded from JSON above it may not be the id that was sent.
const maxTokenID = 1 << 53

// tokenIDs converts one JSON array of token ids to []int64, refusing anything
// that is not a non-negative integer.
func tokenIDs(v []any, path string) ([]int64, error) {
	ids := make([]int64, len(v))
	for j, item := range v {
		f, ok := item.(float64)
		if !ok || f < 0 || f > maxTokenID || f != math.Trunc(f) {
			return nil, EmbeddingInputError("embed: %s[%d] is not a token id; a token array must hold only non-negative integers", path, j)
		}
		ids[j] = int64(f)
	}
	return ids, nil
}

// EmbeddingInputCount is the number of embeddings a normalized input asks for:
// one for a bare string or a single token array, one per element for a
// []string or a [][]int64 of token arrays. A provider compares it against the
// vectors it decoded, because a 2xx body carrying fewer — an error envelope
// with no data at all, or a short list — decodes without error, and returned as
// an answer it hands the caller fewer vectors than it sent texts while the
// target is recorded as having served them.
func EmbeddingInputCount(normalized any) int {
	switch v := normalized.(type) {
	case []string:
		return len(v)
	case [][]int64:
		return len(v)
	default:
		return 1
	}
}

// ValidateEmbeddingEncodingFormat rejects an embeddings encoding_format the
// gateway cannot serve. Empty (unset) and "float" are accepted; any other
// value — "base64" included — is refused.
//
// It returns a 400-carrying *HTTPStatusError rather than a bare error, which is
// what every one of its callers used to return. A bare error carries no status,
// so internal/apierror classified it as a 500 — telling a caller who sent an
// unsupported VALUE that the gateway was broken — and strategies.shouldRetry
// reads a status-less error as a transport failure and retried it, spending the
// whole retry budget on a request whose outcome could not change.
//
// The provider name is empty because this refusal is the gateway's, not any
// upstream's; HTTPStatusError.Error() renders that case without one. The
// caller-facing text is the Message field, and it names the rejected value: for
// a value error that is the only actionable thing in the response, which is why
// this is not an UnsupportedParamError (that type carries parameter NAMES and
// would answer "encoding_format is unsupported" for a parameter that is in fact
// supported, just not at that value).
//
// "base64" is refused rather than decoded because it cannot survive this
// gateway: core.Embedding.Embedding is []float64 and internal/handler/embeddings.go
// JSON-encodes it directly, so the response leaves as a float array whatever the
// upstream sent. Decoding base64 back to floats preserves none of the bandwidth
// saving base64 exists for, while forwarding it upstream returns a vector no
// float-typed decoder can read.
func ValidateEmbeddingEncodingFormat(format string) error {
	if format != "" && format != "float" {
		return StatusError("", http.StatusBadRequest,
			fmt.Sprintf("embed: unsupported encoding_format %q; valid value is %q", format, "float"))
	}
	return nil
}

// coerceAnyStrings converts a []any of strings to []string, rejecting any
// non-string element with a positional error. Token-id input is named as such,
// since it is the one non-text shape the OpenAI contract defines.
func coerceAnyStrings(v []any) ([]string, error) {
	if isTokenInput(v) {
		return nil, EmbeddingInputError("embed: Input is token ids, which this provider cannot embed; send the text instead")
	}
	strs := make([]string, 0, len(v))
	for i, item := range v {
		s, ok := item.(string)
		if !ok {
			return nil, EmbeddingInputError("embed: Input[%d] is %T, want string", i, item)
		}
		strs = append(strs, s)
	}
	return strs, nil
}
