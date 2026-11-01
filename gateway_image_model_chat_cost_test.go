package aigateway

import (
	"context"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

// The Gemini image models — gemini-2.5-flash-image, gemini-3-pro-image-preview
// and the rest — are served through the same generateContent call as every
// other Gemini model, so a chat completion names one as readily as an images
// request does. Their catalog rows carry a per-tile price beside per-token
// rates, and a chat completion counts no images: it reports tokens. Priced on
// the per-tile rate, that zero image count made every such completion a known
// $0.00 — the request-log row, the span and the completed event all recorded a
// call the provider billed as though it had been free.
func TestRoute_ChatOnImageModelIsPricedOnItsTokens(t *testing.T) {
	usage := providers.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000, TotalTokens: 2_000_000}

	t.Run("unary", func(t *testing.T) {
		gw := imageModelChatGateway(t, &mockProvider{
			name:   "tiles",
			models: []string{"image-model"},
			resp: &providers.Response{
				Model:   "image-model",
				Usage:   usage,
				Choices: []providers.Choice{{Message: providers.Message{Role: "assistant", Content: "a rig"}, FinishReason: "stop"}},
			},
		})
		measured := costRecorder(t, gw)

		if _, err := gw.Route(context.Background(), providers.Request{
			Model:    "image-model",
			Messages: []providers.Message{{Role: "user", Content: "draw a rig"}},
		}); err != nil {
			t.Fatalf("Route: %v", err)
		}
		assertImageModelChatCost(t, *measured)
	})

	t.Run("stream", func(t *testing.T) {
		ch := make(chan providers.StreamChunk, 2)
		ch <- providers.StreamChunk{Model: "image-model", Choices: []providers.StreamChoice{{Delta: providers.MessageDelta{Content: "a rig"}}}}
		ch <- providers.StreamChunk{Model: "image-model", Usage: &usage, Choices: []providers.StreamChoice{{FinishReason: "stop"}}}
		close(ch)
		gw := imageModelChatGateway(t, &mockStreamProvider{
			mockProvider: mockProvider{name: "tiles", models: []string{"image-model"}},
			streamCh:     ch,
		})
		measured := costRecorder(t, gw)

		out, err := gw.RouteStream(context.Background(), providers.Request{
			Model:    "image-model",
			Messages: []providers.Message{{Role: "user", Content: "draw a rig"}},
			Stream:   true,
		})
		if err != nil {
			t.Fatalf("RouteStream: %v", err)
		}
		for chunk := range out {
			_ = chunk
		}
		assertImageModelChatCost(t, *measured)
	})
}

func imageModelChatGateway(t *testing.T, p providers.Provider) *Gateway {
	t.Helper()
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "tiles"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	gw.RegisterProvider(p)
	gw.mu.Lock()
	gw.catalog = imageCostCatalog()
	gw.mu.Unlock()
	return gw
}

func assertImageModelChatCost(t *testing.T, measured plugin.Measurements) {
	t.Helper()
	if !measured.HasCost {
		t.Fatal("HasCost = false: the row carries token rates for the tokens the completion reported")
	}
	if measured.CostUSD != 15.0 {
		t.Errorf("CostUSD = %v, want 15.0 (1M input at $5/M + 1M output at $10/M): a completion that generated no counted image was priced per tile", measured.CostUSD)
	}
}
