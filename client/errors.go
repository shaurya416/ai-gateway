package client

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Response header names the gateway sets on every routed surface, naming the
// target that served the request or, on a failure, the last one attempted.
const (
	HeaderGatewayProvider = "X-Gateway-Provider"
	HeaderGatewayTarget   = "X-Gateway-Target"
	HeaderGatewayModel    = "X-Gateway-Model"
	HeaderGatewayAttempts = "X-Gateway-Attempts"
	HeaderRequestID       = "X-Request-ID"
)

// Attribution names the routing target a request reached.
//
// It is empty when no target was attempted: a request a plugin denied, a model
// no target serves, and the endpoints that are not routed (Models,
// Capabilities).
type Attribution struct {
	// Provider is the serving target's canonical provider, e.g. "openai".
	Provider string
	// Target is the target key as configured: targets[].virtual_key.
	Target string
	// Model is the upstream model sent to the provider, after model_map.
	Model string
	// Attempts counts the routing-layer attempts for the request: provider
	// calls plus local circuit-breaker and concurrency refusals, retries and
	// failovers included.
	Attempts int
}

// ResponseMeta describes the HTTP response a result or an error came from.
type ResponseMeta struct {
	StatusCode int
	// Header is the full response header set.
	Header http.Header
	// Attribution is parsed from the X-Gateway-* headers.
	Attribution Attribution
	// RequestID is X-Request-ID: the trace ID the gateway logged the request
	// under, and the one to quote when asking an operator about it.
	RequestID string
}

func metaFrom(resp *http.Response) ResponseMeta {
	attempts, _ := strconv.Atoi(strings.TrimSpace(resp.Header.Get(HeaderGatewayAttempts)))
	return ResponseMeta{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Attribution: Attribution{
			Provider: resp.Header.Get(HeaderGatewayProvider),
			Target:   resp.Header.Get(HeaderGatewayTarget),
			Model:    resp.Header.Get(HeaderGatewayModel),
			Attempts: attempts,
		},
		RequestID: resp.Header.Get(HeaderRequestID),
	}
}

// APIError is a non-2xx answer from the gateway.
//
// Type and Code are the OpenAI error envelope's fields, which the gateway
// keeps stable for clients to switch on: "rate_limit_exceeded",
// "provider_saturated", "model_not_found", "upstream_unavailable",
// "request_rejected", "insufficient_quota", and so on.
type APIError struct {
	StatusCode int
	Type       string
	Code       string
	Message    string
	// Body is the raw response body, up to 1 MiB.
	Body []byte
	// RetryAfter is the wait the Retry-After header asked for, zero when it
	// carried none. The gateway sets it on a 429 it decided itself, and passes
	// through a wait an upstream provider named.
	RetryAfter time.Duration
	// Meta carries the response headers, including the routing attribution of
	// the last target attempted.
	Meta ResponseMeta
}

// Error implements error.
func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "gateway returned %d", e.StatusCode)
	if e.Code != "" {
		fmt.Fprintf(&b, " %s", e.Code)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	if e.RetryAfter > 0 {
		fmt.Fprintf(&b, " (retry after %s)", e.RetryAfter)
	}
	return b.String()
}

// Retryable reports whether the same request may succeed if sent again: a
// timeout, a conflict, a rate limit or a server-side failure. Wait RetryAfter
// first when it is set.
func (e *APIError) Retryable() bool {
	switch {
	case e.StatusCode == http.StatusRequestTimeout,
		e.StatusCode == http.StatusConflict,
		e.StatusCode == http.StatusTooManyRequests:
		return true
	case e.StatusCode >= 500:
		return true
	default:
		return false
	}
}

// errorEnvelope is the OpenAI error body: {"error": {...}}.
type errorEnvelope struct {
	Error *errorBody `json:"error"`
}

type errorBody struct {
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Code    json.RawMessage `json:"code"`
}

// code renders the envelope's code, which the gateway writes as a string and
// some OpenAI-compatible servers write as a number or null.
func (b *errorBody) code() string {
	if len(b.Code) == 0 || string(b.Code) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(b.Code, &s); err == nil {
		return s
	}
	return string(b.Code)
}

func newAPIError(resp *http.Response, meta ResponseMeta) *APIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	e := &APIError{
		StatusCode: resp.StatusCode,
		Body:       body,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		Meta:       meta,
	}
	var envelope errorEnvelope
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error != nil {
		e.Message = envelope.Error.Message
		e.Type = envelope.Error.Type
		e.Code = envelope.Error.code()
		return e
	}
	e.Message = strings.TrimSpace(string(body))
	if len(e.Message) > 512 {
		e.Message = strings.ToValidUTF8(e.Message[:512], "")
	}
	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	return e
}

// parseRetryAfter reads a Retry-After value: delay-seconds or an HTTP-date.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(value, 64); err == nil {
		if secs <= 0 || math.IsNaN(secs) || math.IsInf(secs, 0) {
			return 0
		}
		if secs > float64(math.MaxInt64/int64(time.Second)) {
			return time.Duration(math.MaxInt64)
		}
		return time.Duration(secs * float64(time.Second))
	}
	if at, err := http.ParseTime(value); err == nil {
		if d := at.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// StreamError is an error the gateway reported inside a stream it had already
// opened with a 200: the upstream failed mid-stream ("stream_error") or sent
// nothing for longer than the gateway's idle bound ("stream_timeout").
type StreamError struct {
	Type    string
	Code    string
	Message string
	// Meta carries the headers the stream was opened with, including its
	// routing attribution.
	Meta ResponseMeta
}

// Error implements error.
func (e *StreamError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("gateway stream failed (%s): %s", e.Code, e.Message)
	}
	return "gateway stream failed: " + e.Message
}
