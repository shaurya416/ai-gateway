package aigateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/plugin"
	cacheplugin "github.com/ferro-labs/ai-gateway/plugin/cache"
	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// A response replayed as a stream — a cache hit, or the answer the MCP loop
// settled on — reaches the client in the shape a provider-served stream has.
// The replay forwarded the message's tool calls as the unary response held
// them, with no index, and dropped its reasoning. A streamed tool call is keyed
// by its index, so the Node SDK's stream helper filed both calls below under
// one missing key and finished with finish_reason tool_calls and no tool calls
// at all; the reasoning a non-streamed request for the same answer received
// never arrived.
func TestGateway_RouteStream_ReplayCarriesToolCallIndexesAndReasoning(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "stream"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	gw.RegisterProvider(&mockStreamProvider{
		mockProvider: mockProvider{name: "stream", models: []string{"gpt-4o"}},
		streamErr:    errNilStream,
	})
	served := &providers.Response{
		ID:    "cached",
		Model: "gpt-4o",
		Choices: []providers.Choice{{
			Message: providers.Message{
				Role:             "assistant",
				ReasoningContent: "Two lookups are needed.",
				ToolCalls: []providers.ToolCall{
					{ID: "call_a", Type: "function", Function: providers.FunctionCall{Name: "get_weather", Arguments: `{"city":"Paris"}`}},
					{ID: "call_b", Type: "function", Function: providers.FunctionCall{Name: "get_time", Arguments: `{"tz":"UTC"}`}},
				},
			},
			FinishReason: "tool_calls",
		}},
	}
	if err := gw.RegisterPlugin(plugin.StageBeforeRequest, &testPlugin{
		name: "cache",
		typ:  plugin.TypeLogging,
		execFn: func(_ context.Context, pctx *plugin.Context) error {
			pctx.SkipProvider = true
			pctx.Response = served
			return nil
		},
	}); err != nil {
		t.Fatalf("register plugin: %v", err)
	}

	ch, err := gw.RouteStream(context.Background(), providers.Request{
		Model:    "gpt-4o",
		Stream:   true,
		Messages: []providers.Message{{Role: "user", Content: "weather and time?"}},
	})
	if err != nil {
		t.Fatalf("RouteStream error = %v", err)
	}
	var chunks []providers.StreamChunk
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("stream chunk error: %v", chunk.Error)
		}
		chunks = append(chunks, chunk)
	}
	if len(chunks) != 1 || len(chunks[0].Choices) != 1 {
		t.Fatalf("chunks = %+v, want the replayed answer as one chunk", chunks)
	}
	delta := chunks[0].Choices[0].Delta

	if delta.ReasoningContent != "Two lookups are needed." {
		t.Errorf("reasoning_content = %q, want the response's reasoning", delta.ReasoningContent)
	}
	if len(delta.ToolCalls) != 2 {
		t.Fatalf("tool_calls = %+v, want both calls", delta.ToolCalls)
	}
	for i, call := range delta.ToolCalls {
		if call.Index == nil || *call.Index != i {
			t.Errorf("tool_calls[%d].index = %v, want %d", i, call.Index, i)
		}
	}
	wire, err := json.Marshal(chunks[0])
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}
	if !strings.Contains(string(wire), `"index":1,"id":"call_b"`) {
		t.Errorf("wire chunk %s carries no index for the second tool call", wire)
	}
	// The replay must not stamp the response it was handed: a cache hit replays
	// an entry every other hit reads too.
	for i, call := range served.Choices[0].Message.ToolCalls {
		if call.Index != nil {
			t.Errorf("served tool_calls[%d].index = %d, want the response left untouched", i, *call.Index)
		}
	}
}

// A streamed reasoning answer served again from the response cache keeps its
// reasoning. The stream's assembled response is what the cache stores, and the
// assembly dropped reasoning_content, so every cache hit for a reasoning model
// replayed the answer without it.
func TestGateway_RouteStream_CachedReasoningSurvivesTheReplay(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "stream"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	gw.RegisterProvider(&mockStreamProvider{
		mockProvider: mockProvider{name: "stream", models: []string{"deepseek-reasoner"}},
		streamFn: func(context.Context, providers.Request) (<-chan providers.StreamChunk, error) {
			ch := make(chan providers.StreamChunk, 3)
			ch <- providers.StreamChunk{ID: "chatcmpl-r", Choices: []providers.StreamChoice{{
				Delta: providers.MessageDelta{Role: "assistant", ReasoningContent: "Greeting; reply briefly."},
			}}}
			ch <- providers.StreamChunk{ID: "chatcmpl-r", Choices: []providers.StreamChoice{{
				Delta:        providers.MessageDelta{Content: "Hello"},
				FinishReason: "stop",
			}}}
			ch <- providers.StreamChunk{ID: "chatcmpl-r", Usage: &core.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}}
			close(ch)
			return ch, nil
		},
	})
	cache := &cacheplugin.ResponseCache{}
	if err := cache.Init(map[string]any{"max_age": 60}); err != nil {
		t.Fatalf("cache init: %v", err)
	}
	_ = gw.RegisterPlugin(plugin.StageBeforeRequest, cache)
	_ = gw.RegisterPlugin(plugin.StageAfterRequest, cache)

	req := providers.Request{
		Model:    "deepseek-reasoner",
		Stream:   true,
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	}
	reasoning := func() string {
		t.Helper()
		ch, err := gw.RouteStream(context.Background(), req)
		if err != nil {
			t.Fatalf("RouteStream error = %v", err)
		}
		var text string
		for chunk := range ch {
			if chunk.Error != nil {
				t.Fatalf("stream chunk error: %v", chunk.Error)
			}
			for _, choice := range chunk.Choices {
				text += choice.Delta.ReasoningContent
			}
		}
		return text
	}

	if got := reasoning(); got != "Greeting; reply briefly." {
		t.Fatalf("provider-served reasoning = %q", got)
	}
	if got := reasoning(); got != "Greeting; reply briefly." {
		t.Errorf("cache-served reasoning = %q, want what the provider streamed", got)
	}
}
