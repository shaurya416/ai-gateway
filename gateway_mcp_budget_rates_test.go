package aigateway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/authctx"
	"github.com/ferro-labs/ai-gateway/mcp"
	"github.com/ferro-labs/ai-gateway/models"
	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/plugin/budget"
	"github.com/ferro-labs/ai-gateway/providers"
)

// A budget prices spend against its own configured rates and never against the
// model catalog, and the per-turn check inside an agentic MCP loop has to do the
// same. It was handed the catalog's figure for what the request had spent, so
// on a model the catalog does not price — a self-hosted or declared one — the
// running spend read as nothing and the cap never closed mid-request, and on a
// priced one the check added catalog dollars to a store kept in the budget's
// own. Here the opening turn alone costs $1.20 at the budget's rates against a
// $0.50 cap, so the first loop turn must be refused before a second call, and
// the refusal must record the $1.20 the budget itself charges.
func TestRoute_MCPLoopBudgetChecksSpendAtItsOwnRates(t *testing.T) {
	cases := []struct {
		name    string
		catalog models.Catalog
	}{
		{name: "model the catalog does not price", catalog: models.Catalog{}},
		{name: "model the catalog prices lower", catalog: models.Catalog{
			"mcp-budget-provider/test-model": {
				Provider: "mcp-budget-provider",
				ModelID:  "test-model",
				Mode:     models.ModeChat,
				Pricing: models.Pricing{
					InputPerMTokens:  ptrFloat64(0.01),
					OutputPerMTokens: ptrFloat64(0.01),
				},
			},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mcpSrv := newMCPTestServer(t)
			defer mcpSrv.Close()

			const storeID = "mcp-loop-budget-rates"
			budgetCfg := map[string]any{
				"store_id":            storeID,
				"spend_limit_usd":     0.5,
				"input_per_m_tokens":  10000.0,
				"output_per_m_tokens": 10000.0,
			}
			budget.ResetStore(storeID)
			defer budget.ResetStore(storeID)

			mp := &multiCallProvider{
				name:   "mcp-budget-provider",
				models: []string{"test-model"},
				responses: []*providers.Response{
					mcpToolCallTurn("resp-1", "tc-1", mcpUsageTurns[0]),
					mcpToolCallTurn("resp-2", "tc-2", mcpUsageTurns[1]),
					{
						ID:    "resp-final",
						Model: "test-model",
						Usage: mcpUsageTurns[2],
						Choices: []providers.Choice{{
							Message:      providers.Message{Role: "assistant", Content: "The answer is 42."},
							FinishReason: "stop",
						}},
					},
				},
			}
			gw, err := newTestGateway(t, config.Config{
				Strategy: config.StrategyConfig{Mode: config.ModeSingle},
				Targets:  []config.Target{{VirtualKey: "mcp-budget-provider"}},
				MCPServers: []mcp.ServerConfig{{
					Name:           "test-mcp",
					URL:            mcpSrv.URL + "/mcp",
					TimeoutSeconds: 5,
				}},
				Plugins: []config.PluginConfig{
					{Name: "budget", Type: "ratelimit", Stage: "before_request", Enabled: true, Config: budgetCfg},
					{Name: "budget", Type: "ratelimit", Stage: "after_request", Enabled: true, Config: budgetCfg},
				},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			gw.RegisterProvider(mp)
			gw.catalog = tc.catalog
			if err := gw.LoadPlugins(); err != nil {
				t.Fatalf("LoadPlugins: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			select {
			case <-gw.MCPInitDone():
			case <-ctx.Done():
				t.Fatal("timed out waiting for MCP initialization")
			}
			ctx = authctx.WithKeyID(ctx, "loop-key")
			req := providers.Request{
				Model:    "test-model",
				Messages: []providers.Message{{Role: "user", Content: "What is the answer?"}},
			}

			_, err = gw.Route(ctx, req)
			var rejection *plugin.RejectionError
			if !errors.As(err, &rejection) || rejection.Plugin != "budget" {
				t.Fatalf("Route err = %v, want the budget to refuse the first loop turn: the opening turn cost $1.20 at the budget's rates against a $0.50 cap", err)
			}
			if got := mp.callCount(); got != 1 {
				t.Errorf("provider called %d times, want 1: the loop went on past a cap the opening turn crossed", got)
			}

			// The refusal recorded what the budget charges for the opening turn,
			// so the key's next request is refused on that figure.
			_, err = gw.Route(ctx, req)
			if !errors.As(err, &rejection) || rejection.Plugin != "budget" {
				t.Fatalf("second Route err = %v, want a budget refusal", err)
			}
			if !strings.Contains(rejection.Reason, "spent $1.2000") {
				t.Errorf("second refusal reason = %q, want the $1.2000 the opening turn cost at the budget's own rates", rejection.Reason)
			}
		})
	}
}
