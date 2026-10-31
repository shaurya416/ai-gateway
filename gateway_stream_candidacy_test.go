package aigateway

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/providers"
)

// countingStreamer is a streaming provider that counts its stream starts.
func countingStreamer(name, model string, starts *atomic.Int64) *mockStreamProvider {
	return &mockStreamProvider{
		mockProvider: mockProvider{name: name, models: []string{model}},
		streamFn: func(context.Context, providers.Request) (<-chan providers.StreamChunk, error) {
			starts.Add(1)
			ch := make(chan providers.StreamChunk, 1)
			ch <- providers.StreamChunk{ID: name, Choices: []providers.StreamChoice{{Delta: providers.MessageDelta{Content: "hi"}}}}
			close(ch)
			return ch, nil
		},
	}
}

// Streaming is a surface like embeddings and images: a target whose provider
// cannot stream is not a candidate there, so it must not take a share of a
// weighted draw it can never serve. The stream order was drawn over every
// chat-capable target instead, so the chat-only target won its draws and the
// walk handed each of them to whichever streaming target followed it in the
// rotation — here, all of them to "first" and none to "second".
func TestRouteStream_LoadBalanceWeighsOnlyStreamingTargets(t *testing.T) {
	const model = "stream-model"
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeLoadBalance},
		Targets: []config.Target{
			{VirtualKey: "chat-only", Weight: 1_000_000_000},
			{VirtualKey: "first", Weight: 1},
			{VirtualKey: "second", Weight: 1},
		},
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	var first, second atomic.Int64
	gw.RegisterProvider(&mockProvider{name: "chat-only", models: []string{model}})
	gw.RegisterProvider(countingStreamer("first", model, &first))
	gw.RegisterProvider(countingStreamer("second", model, &second))

	for range 64 {
		ch, streamErr := gw.RouteStream(context.Background(), providers.Request{
			Model:    model,
			Stream:   true,
			Messages: []providers.Message{{Role: roleUser, Content: "hi"}},
		})
		if streamErr != nil {
			t.Fatalf("RouteStream: %v", streamErr)
		}
		drainStream(t, ch)
	}
	if first.Load() == 0 || second.Load() == 0 {
		t.Fatalf("stream starts first=%d second=%d; a target that cannot stream distorted the weights", first.Load(), second.Load())
	}
}

// The chat-only predicates are chat's, and streaming IS chat: narrowing the
// streaming candidates to the targets that can stream must not drop the
// conditional rules that read `stream`, which other surfaces drop.
func TestRouteStream_StreamConditionStillRoutes(t *testing.T) {
	const model = "stream-model"
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{
			Mode: config.ModeConditional,
			Conditions: []config.Condition{
				{Key: config.ConditionKeyStream, Value: "true", TargetKey: "streamer"},
			},
		},
		Targets: []config.Target{{VirtualKey: "default"}, {VirtualKey: "streamer"}},
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	var def, streamer atomic.Int64
	gw.RegisterProvider(countingStreamer("default", model, &def))
	gw.RegisterProvider(countingStreamer("streamer", model, &streamer))

	ch, err := gw.RouteStream(context.Background(), providers.Request{
		Model:    model,
		Stream:   true,
		Messages: []providers.Message{{Role: roleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("RouteStream: %v", err)
	}
	drainStream(t, ch)
	if streamer.Load() != 1 || def.Load() != 0 {
		t.Fatalf("stream starts default=%d streamer=%d; the stream rule must route the request", def.Load(), streamer.Load())
	}
}
