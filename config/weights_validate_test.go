package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A weight the draw cannot honour must not load. The weighted draw sums the
// weights and walks a cumulative total, so a NaN makes every comparison false
// and an infinite weight leaves nothing below it reachable: either way the walk
// falls through to its last entry and the operator's split is inverted. Finite
// weights whose sum overflows to infinity break the draw the same way.
func TestValidateStrategy_NonFiniteWeights(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	runStrategyCases(t, []strategyCase{
		{
			name: "loadbalance NaN weight",
			cfg: Config{
				Strategy: StrategyConfig{Mode: ModeLoadBalance},
				Targets:  []Target{{VirtualKey: "openai", Weight: 9}, {VirtualKey: "groq", Weight: nan}},
			},
			wantErr: `target "groq" has weight NaN; a weight must be a finite number`,
		},
		{
			name: "loadbalance infinite weight",
			cfg: Config{
				Strategy: StrategyConfig{Mode: ModeLoadBalance},
				Targets:  []Target{{VirtualKey: "openai", Weight: inf}, {VirtualKey: "groq", Weight: 1}},
			},
			wantErr: `target "openai" has weight +Inf; a weight must be a finite number`,
		},
		{
			// Already refused as negative; it must stay refused.
			name: "loadbalance negative infinite weight",
			cfg: Config{
				Strategy: StrategyConfig{Mode: ModeLoadBalance},
				Targets:  []Target{{VirtualKey: "openai", Weight: math.Inf(-1)}, {VirtualKey: "groq", Weight: 1}},
			},
			wantErr: "weight -Inf",
		},
		{
			name: "loadbalance weights whose sum overflows",
			cfg: Config{
				Strategy: StrategyConfig{Mode: ModeLoadBalance},
				Targets:  []Target{{VirtualKey: "openai", Weight: math.MaxFloat64}, {VirtualKey: "groq", Weight: math.MaxFloat64}},
			},
			wantErr: "target weights sum past the largest representable number",
		},
		{
			name: "loadbalance large finite weights are legal",
			cfg: Config{
				Strategy: StrategyConfig{Mode: ModeLoadBalance},
				Targets:  []Target{{VirtualKey: "openai", Weight: 1e300}, {VirtualKey: "groq", Weight: 1}},
			},
		},
		{
			name: "ab-test infinite variant weight",
			cfg: Config{
				Strategy: StrategyConfig{Mode: ModeABTest, ABVariants: []ABVariantConfig{
					{TargetKey: "openai", Weight: inf, Label: "control"},
					{TargetKey: "groq", Weight: 1, Label: "challenger"},
				}},
				Targets: twoTargets(),
			},
			wantErr: `ab_variant "control" has weight +Inf`,
		},
		{
			name: "ab-test NaN variant weight",
			cfg: Config{
				Strategy: StrategyConfig{Mode: ModeABTest, ABVariants: []ABVariantConfig{
					{TargetKey: "openai", Weight: 80, Label: "control"},
					{TargetKey: "groq", Weight: nan, Label: "challenger"},
				}},
				Targets: twoTargets(),
			},
			wantErr: `ab_variant "challenger" has weight NaN`,
		},
		{
			// Weights break equal-cost ties under cost-optimized, through the
			// same draw.
			name: "cost-optimized NaN tie-break weight",
			cfg: Config{
				Strategy: StrategyConfig{Mode: ModeCostOptimized},
				Targets:  []Target{{VirtualKey: "openai", Weight: nan}, {VirtualKey: "groq", Weight: 1}},
			},
			wantErr: `target "openai" has weight NaN`,
		},
		{
			name: "cost-optimized all-zero weights stay legal",
			cfg: Config{
				Strategy: StrategyConfig{Mode: ModeCostOptimized},
				Targets:  []Target{{VirtualKey: "openai"}, {VirtualKey: "groq"}},
			},
		},
	})
}

// YAML spells NaN and infinity as `.nan` and `.inf`, and both decode into a
// float without complaint, so the file a deployment actually ships reaches
// validation carrying them.
func TestValidateConfig_RejectsNonFiniteWeightFromYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	body := `strategy:
  mode: load-balance
targets:
  - virtual_key: openai
    weight: 9
  - virtual_key: groq
    weight: .nan
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	err = ValidateConfig(*cfg)
	if err == nil || !strings.Contains(err.Error(), "a weight must be a finite number") {
		t.Fatalf("ValidateConfig = %v, want the NaN weight refused", err)
	}
}
