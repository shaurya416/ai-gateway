package aigateway

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/plugin"
	cacheplugin "github.com/ferro-labs/ai-gateway/plugin/cache"
	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// Token log-probability entries in the shape OpenAI's chat completions return
// them: each streamed choice carries the entries for the tokens of its own
// delta under "content" (and "refusal"), and a non-streamed choice carries every
// token's entry in the same two lists.
const (
	logprobHello = `{"token":"Hello","logprob":-0.1,"bytes":[72,101,108,108,111],"top_logprobs":[]}`
	logprobBang  = `{"token":"!","logprob":-0.2,"bytes":[33],"top_logprobs":[]}`
)

// streamedLogprobs runs one streamed request and returns every logprobs object
// its chunks carried, in order.
func streamedLogprobs(t *testing.T, gw *Gateway, req providers.Request) []json.RawMessage {
	t.Helper()
	ch, err := gw.RouteStream(context.Background(), req)
	if err != nil {
		t.Fatalf("RouteStream error = %v", err)
	}
	var got []json.RawMessage
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("stream chunk error: %v", chunk.Error)
		}
		for _, choice := range chunk.Choices {
			if choice.Logprobs != nil {
				got = append(got, json.RawMessage(choice.Logprobs))
			}
		}
	}
	return got
}

// assertLogprobsJSON fails unless got is the one JSON value want.
func assertLogprobsJSON(t *testing.T, got []json.RawMessage, want string) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("replayed stream carried %d logprobs objects (%s), want one: the request asked for logprobs and the answer has them", len(got), got)
	}
	var g, w any
	if err := json.Unmarshal(got[0], &g); err != nil {
		t.Fatalf("logprobs %s is not JSON: %v", got[0], err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want: %v", err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Errorf("logprobs = %s, want %s", gb, wb)
	}
}

// A response replayed as a stream — a cache hit, or the answer the MCP tool loop
// settled on — carries each choice's logprobs, as a provider-served stream
// does. The replay built every streamed choice from the message alone, so a
// request that asked for logprobs was answered without them: a 200 a client
// cannot tell from a model that returned none.
func TestGateway_RouteStream_ReplayCarriesLogprobs(t *testing.T) {
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
	want := `{"content":[` + logprobHello + `,` + logprobBang + `],"refusal":null}`
	served := &providers.Response{
		ID:    "served",
		Model: "gpt-4o",
		Choices: []providers.Choice{{
			Message:      providers.Message{Role: "assistant", Content: "Hello!"},
			FinishReason: "stop",
			Logprobs:     core.Logprobs(want),
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

	assertLogprobsJSON(t, streamedLogprobs(t, gw, providers.Request{
		Model:    "gpt-4o",
		Stream:   true,
		LogProbs: true,
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	}), want)
}

// A streamed answer served again from the response cache keeps its logprobs.
// The stream's assembled response is what the cache stores, and the assembly
// kept each choice's text but none of the logprobs its chunks carried, so every
// cache hit for a request that asked for them replayed the answer without any.
// The assembled object is the one a non-streamed answer carries: every chunk's
// entries, joined in order.
func TestGateway_RouteStream_CachedLogprobsSurviveTheReplay(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "stream"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	gw.RegisterProvider(&mockStreamProvider{
		mockProvider: mockProvider{name: "stream", models: []string{"gpt-4o"}},
		streamFn: func(context.Context, providers.Request) (<-chan providers.StreamChunk, error) {
			ch := make(chan providers.StreamChunk, 4)
			ch <- providers.StreamChunk{ID: "chatcmpl-l", Choices: []providers.StreamChoice{{
				Delta:    providers.MessageDelta{Role: "assistant", Content: "Hello"},
				Logprobs: core.Logprobs(`{"content":[` + logprobHello + `],"refusal":null}`),
			}}}
			ch <- providers.StreamChunk{ID: "chatcmpl-l", Choices: []providers.StreamChoice{{
				Delta:    providers.MessageDelta{Content: "!"},
				Logprobs: core.Logprobs(`{"content":[` + logprobBang + `],"refusal":null}`),
			}}}
			ch <- providers.StreamChunk{ID: "chatcmpl-l", Choices: []providers.StreamChoice{{
				FinishReason: "stop",
			}}}
			ch <- providers.StreamChunk{ID: "chatcmpl-l", Usage: &core.Usage{PromptTokens: 2, CompletionTokens: 2, TotalTokens: 4}}
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
		Model:    "gpt-4o",
		Stream:   true,
		LogProbs: true,
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	}
	if got := streamedLogprobs(t, gw, req); len(got) != 2 {
		t.Fatalf("provider-served stream carried %d logprobs objects, want 2", len(got))
	}
	assertLogprobsJSON(t, streamedLogprobs(t, gw, req), `{"content":[`+logprobHello+`,`+logprobBang+`],"refusal":null}`)
}
