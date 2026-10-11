package client_test

// The tests in this file run the client against the gateway's own HTTP
// handlers, mounted in-process over a stub provider, so the wire the client
// reads is the one the gateway writes rather than a copy of it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/client"
	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/handler"
	"github.com/ferro-labs/ai-gateway/pkg/logger"
	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/providers/core"
	"github.com/ferro-labs/ai-gateway/test/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunWithEmbeddedCatalog(m.Run))
}

const (
	visibleChat     = "visible-chat"
	upstreamChat    = "upstream-chat"
	visibleEmbed    = "visible-embed"
	throttledModel  = "throttled"
	stubVendor      = "stubvendor"
	upstreamRetryIn = 3 * time.Second
)

// stubProvider answers chat, streaming and embeddings for a fixed set of
// models, and fails the throttled model with an upstream 429.
type stubProvider struct{}

var (
	_ core.StreamProvider    = stubProvider{}
	_ core.EmbeddingProvider = stubProvider{}
)

func (stubProvider) Name() string { return stubVendor }

func (stubProvider) SupportsModel(m string) bool {
	switch m {
	case visibleChat, upstreamChat, visibleEmbed, throttledModel, exampleChatModel, exampleEmbedModel:
		return true
	}
	return false
}

func lastUserText(req providers.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return req.Messages[i].Content
		}
	}
	return ""
}

func (stubProvider) Complete(_ context.Context, req providers.Request) (*providers.Response, error) {
	if req.Model == throttledModel {
		return nil, &core.HTTPStatusError{StatusCode: http.StatusTooManyRequests, Provider: stubVendor, Message: "slow down", RetryAfter: upstreamRetryIn}
	}
	return &providers.Response{
		ID:      "chatcmpl-stub",
		Model:   req.Model,
		Choices: []providers.Choice{{Message: providers.Message{Role: "assistant", Content: "echo: " + lastUserText(req)}, FinishReason: "stop"}},
		Usage:   providers.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
	}, nil
}

func (stubProvider) CompleteStream(_ context.Context, req providers.Request) (<-chan providers.StreamChunk, error) {
	ch := make(chan providers.StreamChunk, 4)
	for _, piece := range []string{"echo", ": ", lastUserText(req)} {
		ch <- providers.StreamChunk{ID: "chatcmpl-stub", Model: req.Model, Choices: []providers.StreamChoice{{Delta: providers.MessageDelta{Content: piece}}}}
	}
	ch <- providers.StreamChunk{ID: "chatcmpl-stub", Model: req.Model, Choices: []providers.StreamChoice{{FinishReason: "stop"}}}
	close(ch)
	return ch, nil
}

func (stubProvider) Embed(_ context.Context, req providers.EmbeddingRequest) (*providers.EmbeddingResponse, error) {
	inputs, err := core.CoerceEmbeddingInput(req.Input)
	if err != nil {
		return nil, err
	}
	resp := &providers.EmbeddingResponse{Object: "list", Model: req.Model}
	for i, in := range inputs {
		resp.Data = append(resp.Data, providers.Embedding{Object: "embedding", Index: i, Embedding: []float64{float64(len(in)), float64(i)}})
	}
	resp.Usage.PromptTokens = len(inputs)
	resp.Usage.TotalTokens = len(inputs)
	return resp, nil
}

// gatewayMux mounts the handlers the gateway binary serves these paths with.
func gatewayMux(gw *aigateway.Gateway) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", handler.ChatCompletions(gw))
	mux.Handle("POST /v1/embeddings", handler.Embeddings(gw))
	mux.Handle("GET /v1/models", handler.Models(gw))
	mux.Handle("GET /v1/capabilities", handler.Capabilities(gw))
	return logger.Middleware(mux)
}

// newGateway builds a gateway over the stub provider, serves it, and returns
// a client for it.
func newGateway(t *testing.T, cfg config.Config, keys ...string) (*aigateway.Gateway, *client.Client) {
	t.Helper()
	gw, err := aigateway.New(cfg)
	if err != nil {
		t.Fatalf("aigateway.New: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })
	for _, key := range keys {
		gw.RegisterProviderAs(key, stubProvider{})
	}
	srv := httptest.NewServer(gatewayMux(gw))
	t.Cleanup(srv.Close)
	c, err := client.New(srv.URL+"/v1", "")
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return gw, c
}

func singleTargetConfig() config.Config {
	return config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets: []config.Target{{
			VirtualKey: "primary",
			Models:     []string{visibleChat, visibleEmbed},
			ModelMap:   map[string]string{visibleChat: upstreamChat},
		}},
	}
}

func chatRequest(model, text string) client.ChatCompletionRequest {
	return client.ChatCompletionRequest{Model: model, Messages: []client.Message{client.UserMessage(text)}}
}

// wantPrimary is the attribution of a chat request the single-target gateway
// serves.
var wantPrimary = client.Attribution{Provider: stubVendor, Target: "primary", Model: upstreamChat, Attempts: 1}

// TestGatewayContractChat reads a unary chat completion and its routing attribution.
func TestGatewayContractChat(t *testing.T) {
	_, c := newGateway(t, singleTargetConfig(), "primary")
	ctx := t.Context()
	resp, err := c.ChatCompletion(ctx, chatRequest(visibleChat, "hello"))
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "echo: hello" || resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("response = %+v", resp)
	}
	if resp.Usage.TotalTokens != 7 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if resp.Meta.Attribution != wantPrimary {
		t.Errorf("attribution = %+v, want %+v", resp.Meta.Attribution, wantPrimary)
	}
	if resp.Meta.RequestID == "" {
		t.Error("no X-Request-ID")
	}
}

// TestGatewayContractStream reads a stream as the gateway writes it, through to its [DONE].
func TestGatewayContractStream(t *testing.T) {
	_, c := newGateway(t, singleTargetConfig(), "primary")
	ctx := t.Context()
	s, err := c.ChatCompletionStream(ctx, chatRequest(visibleChat, "stream me"))
	if err != nil {
		t.Fatalf("ChatCompletionStream: %v", err)
	}
	defer func() { _ = s.Close() }()
	if s.Meta().Attribution != wantPrimary {
		t.Errorf("attribution = %+v, want %+v", s.Meta().Attribution, wantPrimary)
	}
	var acc client.ChatCompletionAccumulator
	for chunk, err := range s.Chunks() {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if chunk.Object != "chat.completion.chunk" || chunk.Choices == nil {
			t.Errorf("chunk = %+v", chunk)
		}
		acc.Add(chunk)
	}
	got := acc.ChatCompletion()
	if got == nil || len(got.Choices) != 1 {
		t.Fatalf("accumulated = %+v", got)
	}
	if m := got.Choices[0].Message; m.Role != client.RoleAssistant || m.Content != "echo: stream me" || got.Choices[0].FinishReason != "stop" {
		t.Errorf("accumulated choice = %+v", got.Choices[0])
	}
}

// TestGatewayContractEmbeddings reads an embeddings response and its attribution.
func TestGatewayContractEmbeddings(t *testing.T) {
	_, c := newGateway(t, singleTargetConfig(), "primary")
	ctx := t.Context()
	resp, err := c.Embeddings(ctx, client.EmbeddingRequest{Model: visibleEmbed, Input: []string{"a", "bcd"}})
	if err != nil {
		t.Fatalf("Embeddings: %v", err)
	}
	if len(resp.Data) != 2 || resp.Data[1].Embedding[0] != 3 || resp.Usage.TotalTokens != 2 {
		t.Errorf("response = %+v", resp)
	}
	if resp.Meta.Attribution.Target != "primary" || resp.Meta.Attribution.Model != visibleEmbed {
		t.Errorf("attribution = %+v", resp.Meta.Attribution)
	}
}

// TestGatewayContractModels reads the model listing, which carries no attribution.
func TestGatewayContractModels(t *testing.T) {
	_, c := newGateway(t, singleTargetConfig(), "primary")
	ctx := t.Context()
	list, err := c.Models(ctx)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	ids := map[string]string{}
	for _, m := range list.Data {
		ids[m.ID] = m.OwnedBy
	}
	if _, ok := ids[visibleChat]; !ok || list.Object != "list" {
		t.Errorf("models = %+v, want the declared %s", list.Data, visibleChat)
	}
	if list.Meta.Attribution != (client.Attribution{}) {
		t.Errorf("an unrouted endpoint carried attribution %+v", list.Meta.Attribution)
	}
}

// TestGatewayContractCapabilities decodes the capability matrix exactly as the handler serves it.
func TestGatewayContractCapabilities(t *testing.T) {
	gw, c := newGateway(t, singleTargetConfig(), "primary")
	ctx := t.Context()
	caps, err := c.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	rec := httptest.NewRecorder()
	handler.Capabilities(gw)(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/capabilities", nil))
	var direct client.Capabilities
	if err := json.Unmarshal(rec.Body.Bytes(), &direct); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(caps.Providers, direct.Providers) || !reflect.DeepEqual(caps.ImageResponseFormats, direct.ImageResponseFormats) {
		t.Errorf("capabilities differ from the handler's own answer:\nclient  %v\nhandler %v", caps.Providers, direct.Providers)
	}
	if len(caps.Providers) == 0 {
		t.Error("no provider profile served")
	}
	for name := range caps.Providers {
		if s, ok := caps.Support(name, "temperature"); !ok || s == "" {
			t.Errorf("Support(%s, temperature) = %q, %v", name, s, ok)
		}
	}
}

// TestGatewayContractModelNotFound reads the 404 for a model no target serves.
func TestGatewayContractModelNotFound(t *testing.T) {
	_, c := newGateway(t, singleTargetConfig(), "primary")
	ctx := t.Context()
	_, err := c.ChatCompletion(ctx, chatRequest("no-such-model", "hi"))
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusNotFound || apiErr.Code != "model_not_found" || apiErr.Type != "invalid_request_error" || apiErr.Retryable() {
		t.Errorf("error = %+v", apiErr)
	}
	if apiErr.Meta.Attribution != (client.Attribution{}) {
		t.Errorf("a request no target served carried attribution %+v", apiErr.Meta.Attribution)
	}
}

// TestGatewayContractStreamRefused reads a stream refused before its first chunk as an *APIError.
func TestGatewayContractStreamRefused(t *testing.T) {
	_, c := newGateway(t, singleTargetConfig(), "primary")
	ctx := t.Context()
	s, err := c.ChatCompletionStream(ctx, chatRequest("no-such-model", "hi"))
	var apiErr *client.APIError
	if s != nil || !errors.As(err, &apiErr) || apiErr.Code != "model_not_found" {
		t.Fatalf("stream %v, err %v", s, err)
	}
}

// TestGatewayContractUpstream429 reads a throttled upstream's 429 through the
// gateway: the status and code the gateway maps it to, the wait the upstream
// named, and the target that refused.
func TestGatewayContractUpstream429(t *testing.T) {
	cfg := config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "primary", Models: []string{throttledModel}}},
	}
	_, c := newGateway(t, cfg, "primary")

	_, err := c.ChatCompletion(t.Context(), chatRequest(throttledModel, "hi"))
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Code != "rate_limit_exceeded" || apiErr.Type != "rate_limit_error" {
		t.Errorf("error = %+v", apiErr)
	}
	if apiErr.RetryAfter != upstreamRetryIn {
		t.Errorf("RetryAfter = %s, want the upstream's %s", apiErr.RetryAfter, upstreamRetryIn)
	}
	if !apiErr.Retryable() {
		t.Error("a 429 must be retryable")
	}
	if got := apiErr.Meta.Attribution; got.Target != "primary" || got.Provider != stubVendor || got.Attempts < 1 {
		t.Errorf("attribution = %+v, want the target that refused", got)
	}
}

// TestGatewayContractRoutingMetadata routes on X-Gateway-Metadata through the
// gateway's conditional strategy, and holds the client's limits to the ones
// the gateway enforces: what the client accepts the gateway reads, and one
// entry or one byte more is refused by both.
func TestGatewayContractRoutingMetadata(t *testing.T) {
	cfg := config.Config{
		Strategy: config.StrategyConfig{
			Mode: config.ModeConditional,
			Conditions: []config.Condition{
				{Key: config.ConditionKeyMetadata, Field: "tier", Value: "gölд", TargetKey: "smart"},
				{Key: config.ConditionKeyMetadata, Field: "priority", Value: "42", TargetKey: "smart"},
			},
		},
		Targets: []config.Target{
			{VirtualKey: "fast", Models: []string{visibleChat}},
			{VirtualKey: "smart", Models: []string{visibleChat}},
		},
	}
	_, c := newGateway(t, cfg, "fast", "smart")
	ctx := t.Context()

	target := func(t *testing.T, opts ...client.RequestOption) string {
		t.Helper()
		resp, err := c.ChatCompletion(ctx, chatRequest(visibleChat, "hi"), opts...)
		if err != nil {
			t.Fatalf("ChatCompletion: %v", err)
		}
		return resp.Meta.Attribution.Target
	}

	if got := target(t); got != "fast" {
		t.Errorf("no metadata routed to %q, want the fallback target", got)
	}
	if got := target(t, client.WithRoutingMetadata(map[string]any{"tier": "gölд"})); got != "smart" {
		t.Errorf("a non-ASCII metadata value routed to %q, want smart", got)
	}
	if got := target(t, client.WithRoutingMetadata(map[string]any{"priority": 42})); got != "smart" {
		t.Errorf("a numeric metadata value routed to %q, want smart", got)
	}
	if got := target(t, client.WithRoutingMetadata(map[string]any{"tier": "bronze"})); got != "fast" {
		t.Errorf("a non-matching value routed to %q, want fast", got)
	}

	s, err := c.ChatCompletionStream(ctx, chatRequest(visibleChat, "hi"), client.WithRoutingMetadata(map[string]any{"tier": "gölд"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Meta().Attribution.Target; got != "smart" {
		t.Errorf("a streamed request routed to %q, want smart", got)
	}
	_ = s.Close()

	entries := func(n int) map[string]any {
		md := make(map[string]any, n)
		for i := 0; i < n; i++ {
			md["k"+strconv.Itoa(i)] = i
		}
		return md
	}
	sized := func(n int) map[string]any {
		// {"k":"…"} is 8 bytes of framing.
		return map[string]any{"k": strings.Repeat("x", n-8)}
	}

	for name, md := range map[string]map[string]any{
		fmt.Sprintf("%d entries", client.MaxRoutingMetadataEntries): entries(client.MaxRoutingMetadataEntries),
		fmt.Sprintf("%d bytes", client.MaxRoutingMetadataBytes):     sized(client.MaxRoutingMetadataBytes),
	} {
		if _, err := c.ChatCompletion(ctx, chatRequest(visibleChat, "hi"), client.WithRoutingMetadata(md)); err != nil {
			t.Errorf("%s: the gateway refused what the client accepted: %v", name, err)
		}
	}

	for name, md := range map[string]map[string]any{
		fmt.Sprintf("%d entries", client.MaxRoutingMetadataEntries+1): entries(client.MaxRoutingMetadataEntries + 1),
		fmt.Sprintf("%d bytes", client.MaxRoutingMetadataBytes+1):     sized(client.MaxRoutingMetadataBytes + 1),
	} {
		if _, err := client.EncodeRoutingMetadata(md); !errors.Is(err, client.ErrInvalidRoutingMetadata) {
			t.Errorf("%s: the client accepted it", name)
		}
		raw, err := json.Marshal(md)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.ChatCompletion(ctx, chatRequest(visibleChat, "hi"), client.WithRequestHeader(client.HeaderRoutingMetadata, string(raw)))
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest || !strings.Contains(apiErr.Message, client.HeaderRoutingMetadata) {
			t.Errorf("%s sent raw: err %v, want the gateway's 400", name, err)
		}
	}
}

// TestGatewayContractConcurrentStreams opens streams from many goroutines on
// one Client, which is documented as safe for concurrent use.
func TestGatewayContractConcurrentStreams(t *testing.T) {
	_, c := newGateway(t, singleTargetConfig(), "primary")
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < cap(errs); i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			text := "n" + strconv.Itoa(i)
			s, err := c.ChatCompletionStream(t.Context(), chatRequest(visibleChat, text))
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = s.Close() }()
			var acc client.ChatCompletionAccumulator
			for s.Next() {
				acc.Add(s.Current())
			}
			if err := s.Err(); err != nil {
				errs <- err
				return
			}
			if got := acc.ChatCompletion().Choices[0].Message.Content; got != "echo: "+text {
				errs <- fmt.Errorf("stream %d read %q", i, got)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
