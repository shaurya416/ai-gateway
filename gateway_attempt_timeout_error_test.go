package aigateway

import (
	"context"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/providers"
)

// TestPipeline_AttemptTimeoutNamesTheTargetOnce pins the message an attempt
// timeout surfaces with. The walk prefixes every error it returns with the
// target that produced it, so an attempt that also named its target read
// "target hung: target hung: attempt timed out …" in logs and error bodies.
func TestPipeline_AttemptTimeoutNamesTheTargetOnce(t *testing.T) {
	for _, mode := range []config.StrategyMode{config.ModeSingle, config.ModeFallback} {
		t.Run(string(mode), func(t *testing.T) {
			gw, err := newTestGateway(t, config.Config{
				Strategy: config.StrategyConfig{Mode: mode},
				Targets:  []config.Target{{VirtualKey: "hung", Timeout: "20ms"}},
			})
			if err != nil {
				t.Fatalf("new gateway: %v", err)
			}
			gw.RegisterProvider(&mockProvider{name: "hung", models: []string{pipelineModel}, completeFn: func(ctx context.Context, _ providers.Request) (*providers.Response, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}})

			_, err = gw.Route(context.Background(), pipelineRequest())
			if err == nil {
				t.Fatal("a hung target answered")
			}
			msg := err.Error()
			if !strings.Contains(msg, "attempt timed out") {
				t.Fatalf("error = %q, want the attempt timeout named", msg)
			}
			if n := strings.Count(msg, "target hung"); n != 1 {
				t.Errorf("error = %q names the target %d times, want once", msg, n)
			}
		})
	}
}
