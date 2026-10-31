package aigateway

import (
	"context"
	"errors"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/plugin"
	_ "github.com/ferro-labs/ai-gateway/plugin/ratelimit"
	"github.com/ferro-labs/ai-gateway/providers"
)

// userRateLimitGateway serves embeddings and images from one target under a
// rate-limit plugin allowing each user one request a minute and everyone
// together far more.
func userRateLimitGateway(t *testing.T) (*Gateway, *mockEmbeddingProvider, *mockImageProvider) {
	t.Helper()
	cfg := map[string]any{"user_rpm": 1}
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeFallback},
		Targets:  []config.Target{{VirtualKey: "embedder"}, {VirtualKey: "painter"}},
		Plugins: []config.PluginConfig{
			{Name: "rate-limit", Type: "ratelimit", Stage: "before_request", Enabled: true, Config: cfg},
		},
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	ep := &mockEmbeddingProvider{mockProvider: mockProvider{name: "embedder", models: []string{"text-embedding-3-small"}}}
	ip := &mockImageProvider{mockProvider: mockProvider{name: "painter", models: []string{"dall-e-3"}}}
	gw.RegisterProvider(ep)
	gw.RegisterProvider(ip)
	if err := gw.LoadPlugins(); err != nil {
		t.Fatalf("load plugins: %v", err)
	}
	return gw, ep, ip
}

func wantUserRateLimited(t *testing.T, surface string, err error) {
	t.Helper()
	var rejection *plugin.RejectionError
	if !errors.As(err, &rejection) || rejection.Reason != "per-user rate limit exceeded" {
		t.Fatalf("%s: second request from the same user = %v, want the per-user rate limit to refuse it", surface, err)
	}
}

// TestGateway_UserRateLimitAppliesToEmbeddingsAndImages covers user_rpm on the
// two non-chat surfaces whose request carries a user. The plugin reads the user
// from Request.User, and the request these surfaces hand the plugin stage
// carried only the model and the content, so a caller limited to one chat
// request a minute could make any number of embeddings or image requests under
// the same user.
func TestGateway_UserRateLimitAppliesToEmbeddingsAndImages(t *testing.T) {
	t.Run("embeddings", func(t *testing.T) {
		gw, ep, _ := userRateLimitGateway(t)
		req := providers.EmbeddingRequest{Model: "text-embedding-3-small", Input: "hello", User: "alice"}
		if _, err := gw.Embed(context.Background(), req); err != nil {
			t.Fatalf("first request: %v", err)
		}
		_, err := gw.Embed(context.Background(), req)
		wantUserRateLimited(t, "embeddings", err)
		if ep.calls != 1 {
			t.Errorf("provider calls = %d, want 1", ep.calls)
		}
		// Another user has a bucket of its own.
		other := req
		other.User = "bob"
		if _, err := gw.Embed(context.Background(), other); err != nil {
			t.Errorf("a different user was refused: %v", err)
		}
	})

	t.Run("images", func(t *testing.T) {
		gw, _, ip := userRateLimitGateway(t)
		req := providers.ImageRequest{Model: "dall-e-3", Prompt: "a lighthouse", User: "alice"}
		if _, err := gw.GenerateImage(context.Background(), req); err != nil {
			t.Fatalf("first request: %v", err)
		}
		_, err := gw.GenerateImage(context.Background(), req)
		wantUserRateLimited(t, "images", err)
		if ip.calls != 1 {
			t.Errorf("provider calls = %d, want 1", ip.calls)
		}
	})
}
