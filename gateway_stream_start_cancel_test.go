package aigateway

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/providers"
)

// endlessStream answers like a provider that has started generating: it sends
// chunks until the context it was started on ends, and closes its channel only
// then. Sends are guarded on ctx the way core.SendChunk guards them. stopped is
// closed once the producer has seen ctx end; halt ends it regardless, so a
// failing run does not leave it generating after the test.
func endlessStream(ctx context.Context, stopped chan<- struct{}, halt <-chan struct{}) <-chan providers.StreamChunk {
	ch := make(chan providers.StreamChunk)
	go func() {
		defer close(ch)
		for {
			chunk := providers.StreamChunk{Choices: []providers.StreamChoice{{Delta: providers.MessageDelta{Content: "token"}}}}
			select {
			case ch <- chunk:
			case <-ctx.Done():
				close(stopped)
				return
			case <-halt:
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return ch
}

// TestGateway_RouteStream_AbandonedStartIsCancelled covers a stream start that
// answers after its wait was abandoned. Nobody will ever read that stream, so
// the generation behind it must be stopped, not drained to completion: drained,
// the upstream produced — and billed — the whole answer for no caller, and held
// the target's only concurrency slot until it finished, shedding every request
// queued behind it. The caller's context here outlives the request, as an
// embedder's does, so nothing else ever ends that generation.
func TestGateway_RouteStream_AbandonedStartIsCancelled(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy:       config.StrategyConfig{Mode: config.ModeSingle},
		RequestTimeout: "50ms",
		Targets: []config.Target{{
			VirtualKey:  mockProviderName,
			Concurrency: &config.ConcurrencyConfig{MaxConcurrency: 1, QueueSize: 1},
		}},
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}

	answer := make(chan struct{})
	stopped := make(chan struct{})
	halt := make(chan struct{})
	t.Cleanup(func() { close(halt) })
	var calls atomic.Int32
	gw.RegisterProvider(&mockStreamProvider{
		mockProvider: mockProvider{name: mockProviderName, models: []string{"gpt-4o"}},
		streamFn: func(ctx context.Context, _ providers.Request) (<-chan providers.StreamChunk, error) {
			if calls.Add(1) == 1 {
				// Overruns request_timeout, then answers with a live stream.
				<-answer
				return endlessStream(ctx, stopped, halt), nil
			}
			return chunkStream("gpt-4o"), nil
		},
	})
	_, lim := resilienceFor(t, gw, mockProviderName)
	if lim == nil {
		t.Fatal("expected a limiter for the target")
	}

	if _, err := gw.RouteStream(context.Background(), streamTestRequest()); !errors.Is(err, ErrRequestTimeout) {
		t.Fatalf("late start = %v, want the gateway's request timeout", err)
	}
	close(answer)

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the abandoned stream kept generating: its start context was never cancelled")
	}
	waitFor(t, func() bool { return len(lim.slots) == 0 })

	ch, err := gw.RouteStream(context.Background(), streamTestRequest())
	if err != nil {
		t.Fatalf("request after the abandoned start = %v, want success: the abandoned stream still held the only slot", err)
	}
	drainMeteredStream(t, ch)
	if got := calls.Load(); got != 2 {
		t.Errorf("provider stream starts = %d, want 2", got)
	}
}

// TestGateway_RouteStream_StartContextOutlivesTheStartAndEndsWithTheStream is
// the other half of the same rule. The context a winning start runs on must
// stay live for as long as its stream does — it is what the provider keeps
// reading the response body on — and must be released once the stream ends,
// so a caller context that outlives many requests does not keep one child
// context per stream it ever served.
func TestGateway_RouteStream_StartContextOutlivesTheStartAndEndsWithTheStream(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy:       config.StrategyConfig{Mode: config.ModeSingle},
		RequestTimeout: "20ms",
		Targets:        []config.Target{{VirtualKey: mockProviderName}},
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}

	streamCtx := make(chan context.Context, 1)
	gw.RegisterProvider(&mockStreamProvider{
		mockProvider: mockProvider{name: mockProviderName, models: []string{"gpt-4o"}},
		streamFn: func(ctx context.Context, _ providers.Request) (<-chan providers.StreamChunk, error) {
			streamCtx <- ctx
			ch := make(chan providers.StreamChunk, 1)
			go func() {
				defer close(ch)
				// Past request_timeout, so a start context ended with the start
				// would already be done here.
				time.Sleep(60 * time.Millisecond)
				if ctx.Err() != nil {
					ch <- providers.StreamChunk{Error: errors.New("stream context ended before the stream did")}
					return
				}
				ch <- providers.StreamChunk{Choices: []providers.StreamChoice{{Delta: providers.MessageDelta{Content: "still streaming"}}}}
			}()
			return ch, nil
		},
	})

	caller, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := gw.RouteStream(caller, streamTestRequest())
	if err != nil {
		t.Fatalf("RouteStream: %v", err)
	}
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("stream failed: %v", chunk.Error)
		}
	}

	ctx := <-streamCtx
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the stream's context was not released when the stream ended")
	}
	if caller.Err() != nil {
		t.Fatal("releasing the stream's context cancelled the caller's")
	}
}
