package repository

import (
	"context"
	"sync"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/providers"
)

// TestScrubConfigSecrets_DoesNotWriteThroughSharedPointers pins the contract
// ScrubConfigSecrets states: the config it is handed is never mutated.
//
// It is handed a shallow copy of the live config — Gateway.GetConfig returns
// the struct by value — so every pointer in it still points at the gateway's
// own retry, circuit-breaker and sticky settings. The walk recursed through
// those pointers and rewrote what they point at: each GET /admin/config
// replaced the live retry policy's status-code slice and re-set the breaker
// timeout and sticky strings, with no lock, while requests read them.
func TestScrubConfigSecrets_DoesNotWriteThroughSharedPointers(t *testing.T) {
	retry := &config.RetryConfig{Attempts: 3, OnStatusCodes: []int{500, 503}}
	breaker := &config.CircuitBreakerConfig{FailureThreshold: 5, Timeout: "30s"}
	sticky := &config.StickyConfig{On: config.StickyOnUser, TTL: "10m"}
	live := config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeFallback, Sticky: sticky},
		Targets:  []config.Target{{VirtualKey: "openai", Retry: retry, CircuitBreaker: breaker}},
	}
	codes := retry.OnStatusCodes

	got := ScrubConfigSecrets(live)

	if &retry.OnStatusCodes[0] != &codes[0] {
		t.Error("the live retry policy's on_status_codes was replaced by the scrub")
	}
	if got.Targets[0].Retry == retry || got.Targets[0].CircuitBreaker == breaker || got.Strategy.Sticky == sticky {
		t.Error("the scrubbed config shares a settings struct with the live one")
	}
	// The copy is still a faithful read of the settings it was taken from.
	if r := got.Targets[0].Retry; r.Attempts != 3 || len(r.OnStatusCodes) != 2 || r.OnStatusCodes[1] != 503 {
		t.Errorf("retry settings were not served as configured: %+v", r)
	}
	if got.Targets[0].CircuitBreaker.Timeout != "30s" || got.Strategy.Sticky.TTL != "10m" {
		t.Errorf("breaker or sticky settings were not served as configured: %+v %+v",
			got.Targets[0].CircuitBreaker, got.Strategy.Sticky)
	}
}

// scrubRaceProvider answers every chat request, so the routing path runs to
// completion and reads the target's retry policy on each call.
type scrubRaceProvider struct{}

func (scrubRaceProvider) Name() string              { return "openai" }
func (scrubRaceProvider) SupportsModel(string) bool { return true }
func (scrubRaceProvider) Complete(context.Context, providers.Request) (*providers.Response, error) {
	return &providers.Response{
		ID:      "chatcmpl-1",
		Model:   "gpt-4o-mini",
		Choices: []providers.Choice{{Message: providers.Message{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
	}, nil
}

// TestScrubConfigSecrets_ConcurrentWithRouting is the same defect as it shows
// in a running gateway: reading the config for GET /admin/config while requests
// are routed. Run under -race (as make test does), the original walk is
// reported writing the retry policy the request path reads.
func TestScrubConfigSecrets_ConcurrentWithRouting(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets: []config.Target{{
			VirtualKey: "openai",
			Models:     []string{"gpt-4o-mini"},
			Retry:      &config.RetryConfig{Attempts: 2, OnStatusCodes: []int{500}},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw.RegisterProvider(scrubRaceProvider{})

	const rounds = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range rounds {
			_ = ScrubConfigSecrets(gw.GetConfig())
		}
	}()
	go func() {
		defer wg.Done()
		req := providers.Request{Model: "gpt-4o-mini", Messages: []providers.Message{{Role: "user", Content: "hi"}}}
		for range rounds {
			if _, err := gw.Route(context.Background(), req); err != nil {
				t.Errorf("Route: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}
