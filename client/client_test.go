package client

import (
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// writeGatewayError writes the error envelope exactly as the gateway's
// apierror.WriteOpenAI does.
func writeGatewayError(w http.ResponseWriter, status int, message, errType, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": message, "type": errType, "code": code},
	})
}

// setAttribution writes the routing attribution headers as
// RoutingAttribution.SetHeaders does.
func setAttribution(h http.Header, provider, target, model string, attempts int) {
	h.Set("X-Gateway-Provider", provider)
	h.Set("X-Gateway-Target", target)
	h.Set("X-Gateway-Model", model)
	h.Set("X-Gateway-Attempts", strconv.Itoa(attempts))
}

// seenHeaders records the headers of the last request a test server handled.
type seenHeaders struct {
	mu sync.Mutex
	h  http.Header
}

func (s *seenHeaders) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h = r.Header.Clone()
}

func (s *seenHeaders) get(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.h.Get(key)
}

func (s *seenHeaders) values(key string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.h.Values(key)
}

// awaitDisconnect blocks until the client has gone away. The request body is
// read first, as the gateway reads it: net/http only watches a connection for
// the client closing it once the body has been consumed.
func awaitDisconnect(r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	<-r.Context().Done()
}

func newTestClient(t *testing.T, handler http.HandlerFunc, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "sk-test", opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNewNormalizesBaseURL(t *testing.T) {
	ok := map[string]string{
		"http://gw:8080":         "http://gw:8080",
		"http://gw:8080/":        "http://gw:8080",
		"http://gw:8080/v1":      "http://gw:8080",
		"https://gw.example/v1/": "https://gw.example",
		"http://gw/ai":           "http://gw/ai",
		"http://gw/ai/v1":        "http://gw/ai",
		"  http://gw:8080  ":     "http://gw:8080",
		"http://ops@gw:8080/v1":  "http://ops@gw:8080",
	}
	for in, want := range ok {
		c, err := New(in, "")
		if err != nil {
			t.Errorf("New(%q): %v", in, err)
			continue
		}
		if c.base != want {
			t.Errorf("New(%q).base = %q, want %q", in, c.base, want)
		}
	}
	for _, bad := range []string{"", "gw:8080", "ftp://gw", "http://", "http://gw?x=1", "http://gw/#f", "http://gw/?"} {
		if _, err := New(bad, ""); err == nil {
			t.Errorf("New(%q) accepted a base URL it must refuse", bad)
		}
	}
}

func TestChatCompletionWire(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != defaultUserAgent {
			t.Errorf("User-Agent = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if _, ok := body["stream"]; ok {
			t.Errorf("unary request carried stream: %v", body["stream"])
		}
		if _, ok := body["stream_options"]; ok {
			t.Errorf("unary request carried stream_options")
		}
		if body["model"] != "gpt-4o-mini" || body["temperature"] != 0.2 || body["max_completion_tokens"] != float64(64) {
			t.Errorf("body = %v", body)
		}
		msgs, _ := body["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("messages = %v", body["messages"])
		}
		if m := msgs[1].(map[string]any); m["role"] != "user" || m["content"] != "Hello" {
			t.Errorf("messages[1] = %v", m)
		}

		setAttribution(w.Header(), "openai", "primary", "gpt-4o-mini-2024-07-18", 2)
		w.Header().Set("X-Request-ID", "req-123")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,"model":"gpt-4o-mini","provider":"openai",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"Hi there"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12,"cache_read_tokens":4,"prompt_tokens_details":{"cached_tokens":4}}}`+"\n")
	})

	resp, err := c.ChatCompletion(t.Context(), ChatCompletionRequest{
		Model:               "gpt-4o-mini",
		Messages:            []Message{SystemMessage("Be brief."), UserMessage("Hello")},
		Temperature:         Ptr(0.2),
		MaxCompletionTokens: Ptr(64),
		Stream:              true, // ignored by ChatCompletion
		StreamOptions:       &StreamOptions{IncludeUsage: true},
	})
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if resp.ID != "chatcmpl-1" || resp.Provider != "openai" || len(resp.Choices) != 1 {
		t.Fatalf("response = %+v", resp)
	}
	if got := resp.Choices[0].Message; got.Role != RoleAssistant || got.Content != "Hi there" || resp.Choices[0].FinishReason != "stop" {
		t.Errorf("choice = %+v", resp.Choices[0])
	}
	if resp.Usage.TotalTokens != 12 || resp.Usage.CacheReadTokens != 4 || resp.Usage.PromptTokensDetails == nil || resp.Usage.PromptTokensDetails.CachedTokens != 4 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	want := Attribution{Provider: "openai", Target: "primary", Model: "gpt-4o-mini-2024-07-18", Attempts: 2}
	if resp.Meta.Attribution != want {
		t.Errorf("attribution = %+v, want %+v", resp.Meta.Attribution, want)
	}
	if resp.Meta.StatusCode != http.StatusOK || resp.Meta.RequestID != "req-123" {
		t.Errorf("meta = %+v", resp.Meta)
	}
}

func TestRateLimitErrorCarriesRetryAfterAndAttribution(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		setAttribution(w.Header(), "openai", "primary", "gpt-4o", 1)
		w.Header().Set("Retry-After", "7")
		writeGatewayError(w, http.StatusTooManyRequests, "rate limit exceeded", "rate_limit_error", "rate_limit_exceeded")
	})

	_, err := c.ChatCompletion(t.Context(), ChatCompletionRequest{Model: "gpt-4o", Messages: []Message{UserMessage("hi")}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %T %v, want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Type != "rate_limit_error" || apiErr.Code != "rate_limit_exceeded" || apiErr.Message != "rate limit exceeded" {
		t.Errorf("error = %+v", apiErr)
	}
	if apiErr.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %s, want 7s", apiErr.RetryAfter)
	}
	if !apiErr.Retryable() {
		t.Error("a 429 must be retryable")
	}
	if apiErr.Meta.Attribution.Target != "primary" || apiErr.Meta.Attribution.Attempts != 1 {
		t.Errorf("attribution = %+v", apiErr.Meta.Attribution)
	}
	if !strings.Contains(string(apiErr.Body), `"rate_limit_exceeded"`) {
		t.Errorf("Body = %q, want the gateway's envelope", apiErr.Body)
	}
	if msg := apiErr.Error(); !strings.Contains(msg, "429") || !strings.Contains(msg, "rate_limit_exceeded") || !strings.Contains(msg, "7s") {
		t.Errorf("Error() = %q", msg)
	}
}

// A redirect is surfaced rather than followed, so the bearer credential is
// never replayed to a location the caller did not name.
func TestRedirectIsReturnedNotFollowed(t *testing.T) {
	var followed atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(elsewhere.Close)

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/v1/models", http.StatusFound)
	})
	_, err := c.Models(t.Context())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %T %v, want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusFound {
		t.Errorf("StatusCode = %d, want 302", apiErr.StatusCode)
	}
	if got := apiErr.Meta.Header.Get("Location"); got != elsewhere.URL+"/v1/models" {
		t.Errorf("Location = %q", got)
	}
	if followed.Load() {
		t.Error("the redirect was followed")
	}
}

func TestAPIErrorBodies(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantMessage string
		wantCode    string
		retryable   bool
	}{
		{"model not found", http.StatusNotFound, `{"error":{"message":"no configured provider serves the requested model","type":"invalid_request_error","code":"model_not_found"}}`, "no configured provider serves the requested model", "model_not_found", false},
		{"numeric code", http.StatusBadRequest, `{"error":{"message":"bad","type":"invalid_request_error","code":400}}`, "bad", "400", false},
		{"null code", http.StatusBadRequest, `{"error":{"message":"bad","type":"invalid_request_error","code":null}}`, "bad", "", false},
		{"html from a proxy", http.StatusBadGateway, "<html>bad gateway</html>", "<html>bad gateway</html>", "", true},
		{"empty", http.StatusServiceUnavailable, "", "Service Unavailable", "", true},
		{"budget", http.StatusPaymentRequired, `{"error":{"message":"budget exceeded","type":"insufficient_quota","code":"insufficient_quota"}}`, "budget exceeded", "insufficient_quota", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			})
			_, err := c.Models(t.Context())
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want *APIError", err)
			}
			if apiErr.StatusCode != tt.status || apiErr.Message != tt.wantMessage || apiErr.Code != tt.wantCode {
				t.Errorf("error = %+v", apiErr)
			}
			if apiErr.Retryable() != tt.retryable {
				t.Errorf("Retryable() = %v, want %v", apiErr.Retryable(), tt.retryable)
			}
			if apiErr.RetryAfter != 0 {
				t.Errorf("RetryAfter = %s with no header", apiErr.RetryAfter)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"-3", 0},
		{"3", 3 * time.Second},
		{" 12 ", 12 * time.Second},
		{"1.5", 1500 * time.Millisecond},
		{"soon", 0},
		{"NaN", 0},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
	}
	for _, tt := range tests {
		if got := parseRetryAfter(tt.in, now); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestHeaders(t *testing.T) {
	var got seenHeaders
	handler := func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
	}

	c := newTestClient(t, handler,
		WithHeader("X-Team", "core"),
		WithHeaders(http.Header{"X-Multi": {"a", "b"}, "User-Agent": {"my-app/1.0"}}),
	)
	if _, err := c.Models(t.Context(), WithRequestHeader("X-Team", "edge"), WithUserID("u-1"), WithSessionID("s-1")); err != nil {
		t.Fatal(err)
	}
	if got.get("X-Team") != "edge" {
		t.Errorf("X-Team = %q, want the per-request value", got.get("X-Team"))
	}
	if v := got.values("X-Multi"); len(v) != 2 {
		t.Errorf("X-Multi = %v", v)
	}
	if got.get("User-Agent") != "my-app/1.0" || got.get("X-User-ID") != "u-1" || got.get("X-Session-ID") != "s-1" {
		t.Errorf("User-Agent %q, X-User-ID %q, X-Session-ID %q", got.get("User-Agent"), got.get("X-User-ID"), got.get("X-Session-ID"))
	}
	if got.get("Content-Type") != "" {
		t.Errorf("a GET carried Content-Type %q", got.get("Content-Type"))
	}

	anon, err := New(c.base, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := anon.Models(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got.get("Authorization") != "" {
		t.Errorf("an empty API key sent Authorization %q", got.get("Authorization"))
	}
}

func TestEmbeddingsWire(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if in, _ := body["input"].([]any); len(in) != 2 || body["dimensions"] != float64(3) {
			t.Errorf("body = %v", body)
		}
		setAttribution(w.Header(), "openai", "embed", "text-embedding-3-small", 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","embedding":[0.1,0.2,0.3],"index":0},`+
			`{"object":"embedding","embedding":[0.4,0.5,0.6],"index":1}],"model":"text-embedding-3-small","usage":{"prompt_tokens":4,"total_tokens":4}}`)
	})
	resp, err := c.Embeddings(t.Context(), EmbeddingRequest{Model: "text-embedding-3-small", Input: []string{"a", "b"}, Dimensions: Ptr(3)})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 2 || resp.Data[1].Embedding[2] != 0.6 || resp.Usage.TotalTokens != 4 {
		t.Errorf("response = %+v", resp)
	}
	if resp.Meta.Attribution.Target != "embed" {
		t.Errorf("attribution = %+v", resp.Meta.Attribution)
	}
}

func TestModelsAndCapabilities(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-4o","object":"model","created":0,"owned_by":"openai","mode":"chat",`+
				`"context_window":128000,"max_output_tokens":16384,"capabilities":["vision","streaming"],"status":"ga"},`+
				`{"id":"my-local","object":"model","created":0,"owned_by":"ollama"}]}`)
		case "/v1/capabilities":
			_, _ = io.WriteString(w, `{"providers":{"anthropic":{"logit_bias":"unsupported","response_format":"translate","temperature":"forward"}},`+
				`"image_response_formats":{"gemini":["b64_json"]}}`)
		default:
			http.NotFound(w, r)
		}
	})

	models, err := c.Models(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(models.Data) != 2 || models.Data[0].ContextWindow != 128000 || models.Data[0].Capabilities[0] != "vision" || models.Data[1].OwnedBy != "ollama" {
		t.Errorf("models = %+v", models)
	}

	caps, err := c.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := caps.Support("anthropic", "logit_bias"); !ok || s != SupportUnsupported {
		t.Errorf("Support(anthropic, logit_bias) = %q, %v", s, ok)
	}
	if s, ok := caps.Support("anthropic", "response_format"); !ok || s != SupportTranslate {
		t.Errorf("Support(anthropic, response_format) = %q, %v", s, ok)
	}
	if _, ok := caps.Support("openai", "temperature"); ok {
		t.Error("Support reported a provider the gateway did not describe")
	}
	if f := caps.ImageResponseFormats["gemini"]; len(f) != 1 || f[0] != "b64_json" {
		t.Errorf("image formats = %v", caps.ImageResponseFormats)
	}
}

func TestUnaryTimeout(t *testing.T) {
	release := make(chan struct{})
	c := newTestClient(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}, WithTimeout(50*time.Millisecond))
	defer close(release)

	start := time.Now()
	_, err := c.Models(t.Context())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}

func TestCallerCancellationIsReportedAsContextError(t *testing.T) {
	c := newTestClient(t, func(_ http.ResponseWriter, r *http.Request) { awaitDisconnect(r) })
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(20*time.Millisecond, cancel)
	_, err := c.ChatCompletion(ctx, ChatCompletionRequest{Model: "m", Messages: []Message{UserMessage("hi")}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRoutingMetadataOption(t *testing.T) {
	var calls atomic.Int32
	var seen seenHeaders
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		seen.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"m","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`)
	})
	req := ChatCompletionRequest{Model: "m", Messages: []Message{UserMessage("hi")}}

	if _, err := c.ChatCompletion(t.Context(), req, WithRoutingMetadata(map[string]any{"tier": "gold", "priority": 2})); err != nil {
		t.Fatal(err)
	}
	if header := seen.get(HeaderRoutingMetadata); header != `{"priority":2,"tier":"gold"}` {
		t.Errorf("%s = %q", HeaderRoutingMetadata, header)
	}

	if _, err := c.ChatCompletion(t.Context(), req, WithRoutingMetadata(nil)); err != nil {
		t.Fatal(err)
	}
	if header := seen.get(HeaderRoutingMetadata); header != "" {
		t.Errorf("an empty map sent %s %q", HeaderRoutingMetadata, header)
	}

	before := calls.Load()
	_, err := c.ChatCompletion(t.Context(), req, WithRoutingMetadata(map[string]any{"nested": map[string]any{"a": 1}}))
	if !errors.Is(err, ErrInvalidRoutingMetadata) {
		t.Fatalf("err = %v, want ErrInvalidRoutingMetadata", err)
	}
	if calls.Load() != before {
		t.Error("an invalid metadata map still sent a request")
	}
}

func TestEncodeRoutingMetadata(t *testing.T) {
	valid := []struct {
		name string
		in   map[string]any
		want string
	}{
		{"empty", map[string]any{}, `{}`},
		{"nil", nil, `{}`},
		{"scalars sorted", map[string]any{"b": true, "a": "x", "c": int64(-3), "d": 1.5, "e": uint8(7), "f": float32(0.5)}, `{"a":"x","b":true,"c":-3,"d":1.5,"e":7,"f":0.5}`},
		{"json.Number kept as written", map[string]any{"id": json.Number("12345678901234567890")}, `{"id":12345678901234567890}`},
		{"non-ascii escaped", map[string]any{"city": "Z\u00fcrich"}, `{"city":"Z\u00fcrich"}`},
		{"astral escaped as a surrogate pair", map[string]any{"m": "go\U0001F680"}, `{"m":"go\ud83d\ude80"}`},
		{"DEL escaped", map[string]any{"k": "a\x7fb"}, `{"k":"a\u007fb"}`},
		{"html escaped by the encoder", map[string]any{"k": "<&>"}, `{"k":"\u003c\u0026\u003e"}`},
	}
	for _, tt := range valid {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EncodeRoutingMetadata(tt.in)
			if err != nil {
				t.Fatalf("EncodeRoutingMetadata: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
			for i := 0; i < len(got); i++ {
				if got[i] < 0x20 || got[i] >= 0x7f {
					t.Fatalf("byte %d of %q is not printable ASCII", i, got)
				}
			}
			var back map[string]any
			if err := json.Unmarshal([]byte(got), &back); err != nil {
				t.Fatalf("not JSON: %v", err)
			}
			for k, v := range tt.in {
				if s, ok := v.(string); ok && back[k] != s {
					t.Errorf("%s round-tripped to %q, want %q", k, back[k], s)
				}
			}
		})
	}

	many := map[string]any{}
	for i := 0; i < MaxRoutingMetadataEntries; i++ {
		many["k"+strconv.Itoa(i)] = i
	}
	if _, err := EncodeRoutingMetadata(many); err != nil {
		t.Errorf("%d entries refused: %v", MaxRoutingMetadataEntries, err)
	}
	many["one-more"] = true

	// {"k":"…"} is 8 bytes of framing around the value.
	atLimit := map[string]any{"k": strings.Repeat("a", MaxRoutingMetadataBytes-8)}
	if got, err := EncodeRoutingMetadata(atLimit); err != nil || len(got) != MaxRoutingMetadataBytes {
		t.Errorf("a value of exactly %d bytes: len %d, err %v", MaxRoutingMetadataBytes, len(got), err)
	}
	// The limit is on the escaped form: 700 × "é" is 1400 bytes of UTF-8 but
	// 4200 bytes once escaped, which is what the gateway measures.
	escapedOver := map[string]any{"k": strings.Repeat("é", 700)}

	invalid := map[string]map[string]any{
		"too many entries":       many,
		"one byte over":          {"k": strings.Repeat("a", MaxRoutingMetadataBytes-7)},
		"over once escaped":      escapedOver,
		"nested object":          {"k": map[string]any{"a": 1}},
		"array":                  {"k": []string{"a"}},
		"null":                   {"k": nil},
		"NaN":                    {"k": nanValue()},
		"infinity":               {"k": float32(infValue())},
		"bad json.Number":        {"k": json.Number("12abc")},
		"json.Number not number": {"k": json.Number("true")},
		"invalid UTF-8 value":    {"k": "\xff"},
		"invalid UTF-8 key":      {"\xff": "v"},
		"struct":                 {"k": struct{}{}},
	}
	for name, md := range invalid {
		if got, err := EncodeRoutingMetadata(md); !errors.Is(err, ErrInvalidRoutingMetadata) {
			t.Errorf("%s: got %q, %v; want ErrInvalidRoutingMetadata", name, got, err)
		}
	}
}

func nanValue() float64 {
	zero := 0.0
	return zero / zero
}

func infValue() float64 {
	zero := 0.0
	return 1 / zero
}

func TestMessageJSON(t *testing.T) {
	encode := func(m Message) string {
		t.Helper()
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := encode(UserMessage("hi")); got != `{"role":"user","content":"hi"}` {
		t.Errorf("text message = %s", got)
	}
	if got := encode(Message{Role: RoleUser}); got != `{"role":"user","content":""}` {
		t.Errorf("empty message = %s", got)
	}
	parts := Message{Role: RoleUser, Content: "ignored", ContentParts: []ContentPart{
		{Type: "text", Text: "what is this?"},
		{Type: "image_url", ImageURL: &ImageURL{URL: "https://example.com/a.png", Detail: "low"}},
	}}
	if got := encode(parts); got != `{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"https://example.com/a.png","detail":"low"}}]}` {
		t.Errorf("multipart message = %s", got)
	}
	call := Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: FunctionCall{Name: "f", Arguments: `{"a":1}`}}}}
	if got := encode(call); got != `{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}` {
		t.Errorf("tool-call message = %s", got)
	}
	if got := encode(ToolMessage("call_1", "42")); got != `{"role":"tool","content":"42","tool_call_id":"call_1"}` {
		t.Errorf("tool message = %s", got)
	}

	decode := func(s string) Message {
		t.Helper()
		var m Message
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("decode %s: %v", s, err)
		}
		return m
	}
	if m := decode(`{"role":"assistant","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"u"}},{"type":"text","text":"b"}]}`); m.Content != "ab" || len(m.ContentParts) != 3 {
		t.Errorf("multipart decode = %+v", m)
	}
	if m := decode(`{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}`); m.Content != "" || len(m.ToolCalls) != 1 {
		t.Errorf("tool-call decode = %+v", m)
	}
	if m := decode(`{"role":"assistant","content":"x","reasoning":"thought"}`); m.ReasoningContent != "thought" {
		t.Errorf("reasoning decode = %+v", m)
	}
	if m := decode(`{"role":"assistant","content":"x","reasoning_content":"canon","reasoning":"alt"}`); m.ReasoningContent != "canon" {
		t.Errorf("reasoning_content must win: %+v", m)
	}
}

// TestPackageImportsOnlyStandardLibrary holds the package to the promise in its
// documentation: importing it must not pull any module into a caller's build.
// Test files are exempt; they never reach an importer.
func TestPackageImportsOnlyStandardLibrary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %s, which is outside the standard library", name, path)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no package files found")
	}
}
