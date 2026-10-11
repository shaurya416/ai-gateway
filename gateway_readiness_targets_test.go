package aigateway

import (
	"fmt"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
)

// TestReadinessIsDerivedFromTargetsNotRegistrations is the regression test for a
// config that deploys entirely green while failing every request: a target names
// a provider that is not registered, so routing has nothing to dispatch to, but
// readiness used to count *registered providers* instead of *routable targets*.
// Nine healthy providers plus one unroutable target reported "ready" while 100%
// of traffic returned 500 "provider not found".
func TestReadinessIsDerivedFromTargetsNotRegistrations(t *testing.T) {
	tests := []struct {
		name string
		// targets configured; registered lists the providers actually present.
		targets      []string
		registered   []string
		wantReady    bool
		wantRoutable map[string]bool
	}{
		{
			name:         "every target resolves",
			targets:      []string{"a", "b"},
			registered:   []string{"a", "b"},
			wantReady:    true,
			wantRoutable: map[string]bool{"a": true, "b": true},
		},
		{
			name:       "sole target resolves to nothing while other providers are healthy",
			targets:    []string{"typo-provider"},
			registered: []string{"a", "b", "c"},
			// The providers are fine; none of them is reachable through this
			// config, so the instance cannot serve a single request.
			wantReady:    false,
			wantRoutable: map[string]bool{"typo-provider": false},
		},
		{
			name:       "one of two targets resolves is degraded, not dead",
			targets:    []string{"a", "typo-provider"},
			registered: []string{"a"},
			// Fallback skips the unroutable target; requests still succeed.
			wantReady:    true,
			wantRoutable: map[string]bool{"a": true, "typo-provider": false},
		},
		{
			name:         "no providers registered at all",
			targets:      []string{"a"},
			registered:   nil,
			wantReady:    false,
			wantRoutable: map[string]bool{"a": false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			targets := make([]config.Target, 0, len(tt.targets))
			for _, name := range tt.targets {
				targets = append(targets, config.Target{VirtualKey: name})
			}
			gw, err := newTestGateway(t, config.Config{
				Strategy: config.StrategyConfig{Mode: config.ModeFallback},
				Targets:  targets,
			})
			if err != nil {
				t.Fatalf("New gateway: %v", err)
			}
			for _, name := range tt.registered {
				gw.RegisterProvider(&mockProvider{name: name})
			}

			got := gw.Readiness()
			if got.Ready != tt.wantReady {
				t.Errorf("Ready = %v, want %v (targets %v, registered %v)",
					got.Ready, tt.wantReady, tt.targets, tt.registered)
			}
			if len(got.Targets) != len(tt.wantRoutable) {
				t.Fatalf("got %d targets, want %d: %+v", len(got.Targets), len(tt.wantRoutable), got.Targets)
			}
			for _, tr := range got.Targets {
				want, ok := tt.wantRoutable[tr.Name]
				if !ok {
					t.Errorf("unexpected target %q", tr.Name)
					continue
				}
				if tr.Routable != want {
					t.Errorf("target %q Routable = %v, want %v", tr.Name, tr.Routable, want)
				}
			}
		})
	}
}

// TestReadinessTargetWithOpenCircuitIsNotRoutable keeps the pre-existing
// circuit-breaker contract intact now that Ready is computed over targets: a
// target whose provider is registered but whose circuit is open cannot carry
// traffic, so it must not hold the instance in rotation on its own.
func TestReadinessTargetWithOpenCircuitIsNotRoutable(t *testing.T) {
	cbCfg := &config.CircuitBreakerConfig{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		MaxHalfThreshold: 1,
		Timeout:          "1m",
	}
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeFallback},
		Targets:  []config.Target{{VirtualKey: "a", CircuitBreaker: cbCfg}},
	})
	if err != nil {
		t.Fatalf("New gateway: %v", err)
	}
	gw.RegisterProvider(&mockProvider{name: "a"})

	cb := gw.circuitBreakers["a"]
	base := time.Now()
	cb.SetNowForTest(func() time.Time { return base })
	cb.RecordFailure()

	got := gw.Readiness()
	if got.Ready {
		t.Error("Ready = true, want false when the only target's circuit is open")
	}
	if len(got.Targets) != 1 || got.Targets[0].Routable {
		t.Errorf("targets = %+v, want the single target reported unroutable", got.Targets)
	}
}

// TestReadinessCountsOnlyTargetsTheStrategyCanReach holds readiness to the
// targets routing can actually use. Counting every configured target let a
// healthy target the strategy never selects — the second target under
// `mode: single`, or one no conditional rule names — keep an instance in
// rotation while every request failed. Among the reachable targets the
// threshold is unchanged: one routable target is enough.
func TestReadinessCountsOnlyTargetsTheStrategyCanReach(t *testing.T) {
	cbCfg := func() *config.CircuitBreakerConfig {
		return &config.CircuitBreakerConfig{
			FailureThreshold: 1,
			SuccessThreshold: 1,
			MaxHalfThreshold: 1,
			Timeout:          "1m",
		}
	}
	tests := []struct {
		name     string
		strategy config.StrategyConfig
		// registered lists the targets with a provider; open lists registered
		// targets whose circuit is tripped.
		registered    []string
		open          []string
		wantReady     bool
		wantReachable map[string]bool
	}{
		{
			name:          "single with an unregistered first target",
			strategy:      config.StrategyConfig{Mode: config.ModeSingle},
			registered:    []string{"b", "c"},
			wantReady:     false,
			wantReachable: map[string]bool{"a": true, "b": false, "c": false},
		},
		{
			name:          "single with an open first circuit",
			strategy:      config.StrategyConfig{Mode: config.ModeSingle},
			registered:    []string{"a", "b", "c"},
			open:          []string{"a"},
			wantReady:     false,
			wantReachable: map[string]bool{"a": true, "b": false, "c": false},
		},
		{
			name:          "single with a healthy first target",
			strategy:      config.StrategyConfig{Mode: config.ModeSingle},
			registered:    []string{"a"},
			wantReady:     true,
			wantReachable: map[string]bool{"a": true, "b": false, "c": false},
		},
		{
			name:          "mode omitted reads as single",
			strategy:      config.StrategyConfig{},
			registered:    []string{"b", "c"},
			wantReady:     false,
			wantReachable: map[string]bool{"a": true, "b": false, "c": false},
		},
		{
			name:          "fallback with one dead target",
			strategy:      config.StrategyConfig{Mode: config.ModeFallback},
			registered:    []string{"b"},
			wantReady:     true,
			wantReachable: map[string]bool{"a": true, "b": true, "c": true},
		},
		{
			name:          "load-balance with an open first circuit",
			strategy:      config.StrategyConfig{Mode: config.ModeLoadBalance},
			registered:    []string{"a", "c"},
			open:          []string{"a"},
			wantReady:     true,
			wantReachable: map[string]bool{"a": true, "b": true, "c": true},
		},
		{
			name: "conditional whose rule and fallback are both dead",
			strategy: config.StrategyConfig{Mode: config.ModeConditional, Conditions: []config.Condition{
				{Key: config.ConditionKeyModel, Value: "m", TargetKey: "b"},
			}},
			registered:    []string{"b", "c"},
			open:          []string{"b"},
			wantReady:     false,
			wantReachable: map[string]bool{"a": true, "b": true, "c": false},
		},
		{
			name: "conditional chain with one live member",
			strategy: config.StrategyConfig{Mode: config.ModeConditional, Conditions: []config.Condition{
				{Key: config.ConditionKeyModel, Value: "m", TargetKeys: []string{"b", "c"}},
			}},
			registered:    []string{"c"},
			wantReady:     true,
			wantReachable: map[string]bool{"a": true, "b": true, "c": true},
		},
		{
			name: "content-based rule naming a live target",
			strategy: config.StrategyConfig{Mode: config.ModeContentBased, ContentConditions: []config.ContentCondition{
				{Type: config.ContentConditionPromptContains, Value: "python", TargetKey: "c"},
			}},
			registered:    []string{"c"},
			wantReady:     true,
			wantReachable: map[string]bool{"a": true, "b": false, "c": true},
		},
		{
			name: "content-based with only an unnamed target alive",
			strategy: config.StrategyConfig{Mode: config.ModeContentBased, ContentConditions: []config.ContentCondition{
				{Type: config.ContentConditionPromptContains, Value: "python", TargetKey: "c"},
			}},
			registered:    []string{"b"},
			wantReady:     false,
			wantReachable: map[string]bool{"a": true, "b": false, "c": true},
		},
		{
			name: "ab-test falls back to the whole target list",
			strategy: config.StrategyConfig{Mode: config.ModeABTest, ABVariants: []config.ABVariantConfig{
				{TargetKey: "a", Weight: 1, Label: "control"},
			}},
			registered:    []string{"c"},
			wantReady:     true,
			wantReachable: map[string]bool{"a": true, "b": true, "c": true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw, err := newTestGateway(t, config.Config{
				Strategy: tt.strategy,
				Targets: []config.Target{
					{VirtualKey: "a", Weight: 1, CircuitBreaker: cbCfg()},
					{VirtualKey: "b", Weight: 1, CircuitBreaker: cbCfg()},
					{VirtualKey: "c", Weight: 1, CircuitBreaker: cbCfg()},
				},
			})
			if err != nil {
				t.Fatalf("New gateway: %v", err)
			}
			for _, name := range tt.registered {
				gw.RegisterProvider(&mockProvider{name: name, models: []string{"m"}})
			}
			for _, name := range tt.open {
				cb := gw.circuitBreakers[name]
				base := time.Now()
				cb.SetNowForTest(func() time.Time { return base })
				cb.RecordFailure()
			}

			got := gw.Readiness()
			if got.Ready != tt.wantReady {
				t.Errorf("Ready = %v, want %v: %+v", got.Ready, tt.wantReady, got.Targets)
			}
			if len(got.Targets) != len(tt.wantReachable) {
				t.Fatalf("got %d targets, want %d: %+v", len(got.Targets), len(tt.wantReachable), got.Targets)
			}
			for _, tr := range got.Targets {
				if want := tt.wantReachable[tr.Name]; tr.Reachable != want {
					t.Errorf("target %q Reachable = %v, want %v", tr.Name, tr.Reachable, want)
				}
			}
		})
	}
}

// TestReadinessReachabilityMatchesTheConfigItReports keeps a readiness snapshot
// internally consistent across a reload: the strategy is built outside the read
// lock, so without care it can answer for the config a reload just replaced and
// mark the target `single` now selects as unreachable.
func TestReadinessReachabilityMatchesTheConfigItReports(t *testing.T) {
	cfgFor := func(first, second string) config.Config {
		return config.Config{
			Strategy: config.StrategyConfig{Mode: config.ModeSingle},
			Targets:  []config.Target{{VirtualKey: first}, {VirtualKey: second}},
		}
	}
	gw, err := newTestGateway(t, cfgFor("a", "b"))
	if err != nil {
		t.Fatalf("New gateway: %v", err)
	}
	gw.RegisterProvider(&mockProvider{name: "a", models: []string{"m"}})

	done := make(chan struct{})
	stop := make(chan struct{})
	var reloadErr error
	go func() {
		defer close(done)
		for i := range 200 {
			select {
			case <-stop:
				return
			default:
			}
			cfg := cfgFor("a", "b")
			if i%2 == 1 {
				cfg = cfgFor("b", "a")
			}
			if reloadErr = gw.ReloadConfig(t.Context(), cfg); reloadErr != nil {
				return
			}
		}
	}()

	var inconsistent string
	for inconsistent == "" {
		select {
		case <-done:
			if reloadErr != nil {
				t.Fatalf("ReloadConfig: %v", reloadErr)
			}
			return
		default:
		}
		got := gw.Readiness()
		switch {
		case len(got.Targets) != 2:
			inconsistent = fmt.Sprintf("targets = %+v, want two", got.Targets)
		case !got.Targets[0].Reachable || got.Targets[1].Reachable:
			inconsistent = fmt.Sprintf("single must reach exactly its first target, got %+v", got.Targets)
		case got.Ready != got.Targets[0].Routable:
			inconsistent = fmt.Sprintf("Ready = %v, want the first target's routability: %+v", got.Ready, got.Targets)
		}
	}
	close(stop)
	<-done
	t.Fatal(inconsistent)
}
