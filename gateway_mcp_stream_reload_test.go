package aigateway

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/mcp"
	"github.com/ferro-labs/ai-gateway/providers"
)

// A stream RouteStream diverts into the MCP tool loop must be offered the
// tools that diverted it. The decision was made on the registry RouteStream
// read, and the diverted request then read the gateway's registry afresh, so a
// config reload landing between the two — every reload builds a new registry,
// whose servers have not yet completed their handshake — turned the stream
// into a plain non-streaming call with no tools: the caller got one buffered
// chunk, under request_timeout, from a request MCP took no part in.
func TestRouteStream_ReloadDoesNotDivertAStreamPastMCP(t *testing.T) {
	srv := newMCPTestServer(t)
	defer srv.Close()

	var unaryWithoutTools, unaryWithTools atomic.Int64
	p := &mockStreamProvider{mockProvider: mockProvider{name: "p", models: []string{"gpt-4o"}}}
	p.completeFn = func(_ context.Context, req providers.Request) (*providers.Response, error) {
		if len(req.Tools) == 0 {
			unaryWithoutTools.Add(1)
		} else {
			unaryWithTools.Add(1)
		}
		return &providers.Response{
			ID:      "r",
			Model:   "gpt-4o",
			Choices: []providers.Choice{{Message: providers.Message{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
		}, nil
	}

	cfg := config.Config{
		Strategy:   config.StrategyConfig{Mode: config.ModeSingle},
		Targets:    []config.Target{{VirtualKey: "p"}},
		MCPServers: []mcp.ServerConfig{{Name: "tools", URL: srv.URL, TimeoutSeconds: 5}},
	}
	gw, err := newTestGateway(t, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw.RegisterProvider(p)
	select {
	case <-gw.MCPInitDone():
	case <-time.After(5 * time.Second):
		t.Fatal("MCP init timeout")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				ch, err := gw.RouteStream(context.Background(), providers.Request{
					Model:    "gpt-4o",
					Stream:   true,
					Messages: []providers.Message{{Role: "user", Content: "hi"}},
				})
				if err == nil {
					for range ch { //nolint:revive // drain the stream
					}
				}
			}
		}()
	}
	// The same config, reloaded: each reload swaps in a fresh registry and the
	// next one waits for its handshake, so streams keep being diverted.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			if err := gw.ReloadConfig(context.Background(), cfg); err != nil {
				t.Errorf("reload: %v", err)
				return
			}
			select {
			case <-gw.MCPInitDone():
			case <-ctx.Done():
			}
		}
	}()
	wg.Wait()

	if n := unaryWithoutTools.Load(); n != 0 {
		t.Fatalf("%d streamed requests were diverted into a non-streaming call offered no MCP tools (%d were offered them)", n, unaryWithTools.Load())
	}
	if unaryWithTools.Load() == 0 {
		t.Fatal("no stream was diverted into the MCP tool loop")
	}
}
