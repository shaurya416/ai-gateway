// Package client is the Go client for a Ferro Labs AI Gateway.
//
// It speaks the gateway's OpenAI-compatible HTTP API — chat completions
// (unary and streamed), embeddings, the model listing and the capability
// matrix — and surfaces what is particular to this gateway: the routing
// attribution headers on every response, typed errors carrying the gateway's
// error envelope and any Retry-After hint, and the X-Gateway-Metadata request
// header that conditional routing reads.
//
// The package imports nothing outside the standard library, so depending on
// it does not pull the gateway's own dependency graph into a caller's build.
//
//	c, err := client.New("http://localhost:8080", os.Getenv("FERRO_API_KEY"))
//	if err != nil { ... }
//	resp, err := c.ChatCompletion(ctx, client.ChatCompletionRequest{
//		Model:    "gpt-4o-mini",
//		Messages: []client.Message{client.UserMessage("Hello")},
//	})
//	fmt.Println(resp.Choices[0].Message.Content, resp.Meta.Attribution.Target)
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Paths of the gateway operations this client calls, relative to the API root.
const (
	pathChatCompletions = "/v1/chat/completions"
	pathEmbeddings      = "/v1/embeddings"
	pathModels          = "/v1/models"
	pathCapabilities    = "/v1/capabilities"
)

// defaultUserAgent identifies this client to the gateway. WithHeader replaces it.
const defaultUserAgent = "ferro-ai-gateway-go-client"

// maxResponseBytes bounds how much of a unary response body is read. A chat
// completion, an embeddings batch or the model listing is far below it; the
// bound exists so a misbehaving endpoint cannot make the client buffer
// without limit.
const maxResponseBytes = 64 << 20

// maxErrorBodyBytes bounds how much of an error response body is kept.
const maxErrorBodyBytes = 1 << 20

// Client calls one gateway. It is safe for concurrent use.
type Client struct {
	base       string // scheme://host[/prefix], no trailing slash
	apiKey     string
	httpClient *http.Client
	timeout    time.Duration
	headers    http.Header
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the http.Client requests are sent with. The default is a
// new http.Client using http.DefaultTransport that does not follow redirects:
// a 3xx answer is returned as an *APIError, so the credential is never
// replayed to a location the caller did not name. A client given here keeps
// its own redirect policy.
//
// http.Client.Timeout bounds a whole exchange, response body included, so a
// non-zero value also cuts off a stream that legitimately runs longer than it.
// Use WithTimeout for a bound that does not.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithTimeout bounds each call. For ChatCompletion, Embeddings, Models and
// Capabilities it is a deadline on the whole call. For ChatCompletionStream it
// bounds only opening the stream — until the response headers arrive — because
// a stream's length is the model's to decide; bound a stream's lifetime with
// the context passed to it. Zero, the default, sets no bound beyond the
// context's own.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// WithHeader adds a header sent on every request. It may be given more than
// once; a later value for the same key replaces an earlier one.
func WithHeader(key, value string) Option {
	return func(c *Client) {
		c.headers.Set(key, value)
	}
}

// WithHeaders adds every header in h to each request, replacing earlier values
// for the same keys.
func WithHeaders(h http.Header) Option {
	return func(c *Client) {
		for key, values := range h {
			c.headers.Del(key)
			for _, v := range values {
				c.headers.Add(key, v)
			}
		}
	}
}

// New returns a Client for the gateway at baseURL, authenticating with apiKey
// as a bearer token. An empty apiKey sends no Authorization header.
//
// baseURL is the gateway's root — http://localhost:8080 — or that root with a
// path prefix when the gateway is mounted beneath one. Its API root with the
// version segment, http://localhost:8080/v1, is accepted as the same thing,
// since that is how OpenAI-compatible clients are usually configured. A query
// string or fragment is refused: an operation path is appended to the base, so
// neither could survive into the request that was meant.
func New(baseURL, apiKey string, opts ...Option) (*Client, error) {
	base, err := normalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	c := &Client{
		base:       base,
		apiKey:     apiKey,
		httpClient: &http.Client{CheckRedirect: refuseRedirect},
		headers:    http.Header{"User-Agent": {defaultUserAgent}},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c, nil
}

// refuseRedirect is the default client's redirect policy: the 3xx response
// itself is returned rather than followed.
func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func normalizeBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("client: invalid base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("client: base URL %q must use http or https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("client: base URL %q has no host", raw)
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("client: base URL %q must not carry a query or fragment", raw)
	}
	path := strings.TrimRight(u.Path, "/")
	path = strings.TrimSuffix(path, "/v1")
	u.Path = path
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

// RequestOption configures a single call: a header for this request only, or
// the routing metadata conditional routing reads.
type RequestOption func(*requestConfig) error

type requestConfig struct {
	headers http.Header
}

// WithRequestHeader sets a header on this request only, replacing a value the
// Client would otherwise send for the same key.
func WithRequestHeader(key, value string) RequestOption {
	return func(rc *requestConfig) error {
		rc.headers.Set(key, value)
		return nil
	}
}

// WithUserID sets X-User-ID, the end-user identity the gateway records on the
// request span and the request log.
func WithUserID(id string) RequestOption {
	return WithRequestHeader("X-User-ID", id)
}

// WithSessionID sets X-Session-ID, the session identity the gateway records on
// the request span and the request log.
func WithSessionID(id string) RequestOption {
	return WithRequestHeader("X-Session-ID", id)
}

// WithRoutingMetadata sends md as the X-Gateway-Metadata header, the one
// request header a conditional routing rule may read (key: metadata). The
// gateway reads it on /v1/chat/completions and /v1/completions.
//
// md is validated before anything is sent, against the limits the gateway
// enforces: see EncodeRoutingMetadata. A map that fails them makes the call
// return that error without a request being made. An empty map sends nothing.
func WithRoutingMetadata(md map[string]any) RequestOption {
	return func(rc *requestConfig) error {
		if len(md) == 0 {
			return nil
		}
		value, err := EncodeRoutingMetadata(md)
		if err != nil {
			return err
		}
		rc.headers.Set(HeaderRoutingMetadata, value)
		return nil
	}
}

// newRequest builds an HTTP request for path, with the Client's headers, the
// credential and the per-call options applied in that order.
func (c *Client) newRequest(ctx context.Context, method, path string, body any, accept string, opts []RequestOption) (*http.Request, error) {
	rc := requestConfig{headers: http.Header{}}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(&rc); err != nil {
			return nil, err
		}
	}

	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("client: encode request: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, fmt.Errorf("client: build request: %w", err)
	}
	for key, values := range c.headers {
		req.Header[key] = append([]string(nil), values...)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	for key, values := range rc.headers {
		req.Header[key] = append([]string(nil), values...)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", accept)
	return req, nil
}

// doJSON sends a request and decodes a 2xx JSON body into out. Any other
// status is returned as an *APIError.
func (c *Client) doJSON(ctx context.Context, method, path string, body, out any, opts []RequestOption) (ResponseMeta, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	req, err := c.newRequest(ctx, method, path, body, "application/json", opts)
	if err != nil {
		return ResponseMeta{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ResponseMeta{}, transportError(ctx, err)
	}
	defer func() {
		// Drain a bounded remainder so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()

	meta := metaFrom(resp)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return meta, newAPIError(resp, meta)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return meta, transportError(ctx, err)
	}
	if len(data) > maxResponseBytes {
		return meta, fmt.Errorf("client: response body exceeds %d bytes", maxResponseBytes)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return meta, fmt.Errorf("client: decode %s response: %w", path, err)
	}
	return meta, nil
}

// transportError prefers the context's own error, so a cancelled or expired
// call reports context.Canceled or context.DeadlineExceeded through errors.Is
// rather than whatever the transport made of it.
func transportError(ctx context.Context, err error) error {
	if ctxErr := context.Cause(ctx); ctxErr != nil && !errors.Is(err, ctxErr) {
		return fmt.Errorf("client: %w (%w)", ctxErr, err)
	}
	return fmt.Errorf("client: %w", err)
}

// drainAndClose reads a bounded remainder of body before closing it, so the
// connection can be reused for the next request.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	_ = body.Close()
}

// ChatCompletion sends a chat completion request and returns the gateway's
// response. req.Stream is ignored; use ChatCompletionStream to stream.
func (c *Client) ChatCompletion(ctx context.Context, req ChatCompletionRequest, opts ...RequestOption) (*ChatCompletion, error) {
	req.Stream = false
	req.StreamOptions = nil
	var out ChatCompletion
	meta, err := c.doJSON(ctx, http.MethodPost, pathChatCompletions, &req, &out, opts)
	if err != nil {
		return nil, err
	}
	out.Meta = meta
	return &out, nil
}

// Embeddings sends an embeddings request.
func (c *Client) Embeddings(ctx context.Context, req EmbeddingRequest, opts ...RequestOption) (*EmbeddingResponse, error) {
	var out EmbeddingResponse
	meta, err := c.doJSON(ctx, http.MethodPost, pathEmbeddings, &req, &out, opts)
	if err != nil {
		return nil, err
	}
	out.Meta = meta
	return &out, nil
}

// Models lists the models the gateway advertises (GET /v1/models): every model
// a configured target can be routed to, with catalog metadata where the
// gateway's catalog knows the model.
func (c *Client) Models(ctx context.Context, opts ...RequestOption) (*ModelList, error) {
	var out ModelList
	meta, err := c.doJSON(ctx, http.MethodGet, pathModels, nil, &out, opts)
	if err != nil {
		return nil, err
	}
	out.Meta = meta
	return &out, nil
}

// Capabilities returns the per-provider parameter support the gateway serves
// (GET /v1/capabilities), covering the providers its configured targets name.
func (c *Client) Capabilities(ctx context.Context, opts ...RequestOption) (*Capabilities, error) {
	var out Capabilities
	meta, err := c.doJSON(ctx, http.MethodGet, pathCapabilities, nil, &out, opts)
	if err != nil {
		return nil, err
	}
	out.Meta = meta
	return &out, nil
}
