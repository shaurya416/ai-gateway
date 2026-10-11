package strategies

import (
	"slices"
	"testing"

	"github.com/ferro-labs/ai-gateway/internal/latency"
	"github.com/ferro-labs/ai-gateway/providers"
)

// reachabilityRequests is a battery of requests varied along every field a
// strategy reads: model (served, rule-matched, served by nobody), user, stream,
// tools, routing metadata and prompt content.
func reachabilityRequests() []providers.Request {
	out := make([]providers.Request, 0, 3*3*2*2*2*3)
	for _, model := range []string{"m", "gpt-4o", "served-by-nobody"} {
		for _, user := range []string{"", "u1", "u2"} {
			for _, stream := range []bool{false, true} {
				for _, tools := range [][]providers.Tool{nil, {{Type: "function"}}} {
					for _, meta := range []map[string]string{nil, {"tier": "gold"}} {
						for _, prompt := range []string{"", "teach me python", "rust please"} {
							req := providers.Request{
								Model:           model,
								User:            user,
								Stream:          stream,
								Tools:           tools,
								RoutingMetadata: meta,
							}
							if prompt != "" {
								req.Messages = []providers.Message{{Role: "user", Content: prompt}}
							}
							out = append(out, req)
						}
					}
				}
			}
		}
	}
	return out
}

// TestReachableIsExactlyWhatSelectTargetsReturns holds Reachable to its
// contract with SelectTargets, which readiness relies on: over requests that
// exercise every rule, the keys SelectTargets returns are exactly the keys
// Reachable names. A key SelectTargets returns that Reachable omits is a target
// readiness ignores while traffic reaches it; a key Reachable names that
// SelectTargets never returns is a target that can hold an instance in
// rotation while every request fails.
func TestReachableIsExactlyWhatSelectTargetsReturns(t *testing.T) {
	targets := []Target{
		{VirtualKey: "a", Weight: 1},
		{VirtualKey: "b", Weight: 1},
		{VirtualKey: "c", Weight: 0},
		{VirtualKey: "d", Weight: 1},
		{VirtualKey: "e", Weight: 1},
	}
	lookup := newLookup(
		&mockProvider{name: "a", models: []string{"m", "gpt-4o"}},
		&mockProvider{name: "b", models: []string{"m", "gpt-4o"}},
		&mockProvider{name: "c", models: []string{"m", "gpt-4o"}},
		&mockProvider{name: "d", models: []string{"m", "gpt-4o"}},
		&mockProvider{name: "e", models: []string{"m", "gpt-4o"}},
	)

	type strategyCase struct {
		name  string
		build func(t *testing.T) Strategy
		// want is the reachable set, stated independently of either method.
		want []string
	}
	fixed := []strategyCase{
		{"conditional rules", func(_ *testing.T) Strategy {
			return NewConditional([]ConditionRule{
				{Key: ConditionKeyModel, Value: "gpt-4o", Targets: []Target{{VirtualKey: "b"}}},
				{Key: ConditionKeyMetadata, Field: "tier", Value: "gold", Targets: []Target{{VirtualKey: "c"}, {VirtualKey: "b"}}},
				{Key: ConditionKeyStream, Value: "true", Targets: []Target{{VirtualKey: "d"}}},
			}, targets[0])
		}, []string{"a", "b", "c", "d"}},
		{"content-based rules", func(t *testing.T) Strategy {
			s, err := NewContentBased([]ContentRule{
				{Type: PromptContains, Value: "python", Targets: []Target{{VirtualKey: "b"}}},
				{Type: PromptRegex, Value: "(?i)rust", Targets: []Target{{VirtualKey: "c"}, {VirtualKey: "d"}}},
			}, targets[0])
			if err != nil {
				t.Fatal(err)
			}
			return s
		}, []string{"a", "b", "c", "d"}},
		{"ab-test with routing targets", func(t *testing.T) Strategy {
			s, err := NewABTest([]ABTestVariant{
				{Target: targets[1], Weight: 1, Label: "control"},
				{Target: targets[3], Weight: 0, Label: "drained"},
			}, lookup)
			if err != nil {
				t.Fatal(err)
			}
			return s.WithRoutingTargets(targets)
		}, []string{"a", "b", "c", "d", "e"}},
		{"ab-test drained variant without routing targets", func(t *testing.T) Strategy {
			s, err := NewABTest([]ABTestVariant{
				{Target: targets[1], Weight: 1, Label: "control"},
				{Target: targets[3], Weight: 0, Label: "drained"},
			}, lookup)
			if err != nil {
				t.Fatal(err)
			}
			return s
		}, []string{"b"}},
		{"load-balance with a zero weight", func(_ *testing.T) Strategy {
			return NewLoadBalance(targets, lookup)
		}, []string{"a", "b", "c", "d", "e"}},
		{"least-latency", func(_ *testing.T) Strategy {
			return NewLeastLatency(targets, lookup, latency.New(10))
		}, []string{"a", "b", "c", "d", "e"}},
		{"cost-optimized", func(_ *testing.T) Strategy {
			return NewCostOptimized(targets, lookup, buildCatalog())
		}, []string{"a", "b", "c", "d", "e"}},
	}
	// Every strategy in the package, as strategyCases builds it, so one added
	// there is held to the same contract here.
	every := strategyCases(targets)
	cases := make([]strategyCase, 0, len(fixed)+len(every))
	cases = append(cases, fixed...)
	for _, sc := range every {
		want := []string{"a"}
		if sc.name == "fallback" || sc.name == "loadbalance" || sc.name == "least-latency" || sc.name == "cost-optimized" {
			want = []string{"a", "b", "c", "d", "e"}
		}
		cases = append(cases, strategyCase{
			name:  "strategyCases/" + sc.name,
			build: func(t *testing.T) Strategy { return sc.build(t, lookup) },
			want:  want,
		})
	}

	requests := reachabilityRequests()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.build(t)

			reach := s.Reachable()
			for i, key := range reach {
				if slices.Contains(reach[:i], key) {
					t.Fatalf("Reachable() = %v repeats %q", reach, key)
				}
			}
			assertSameKeys(t, "Reachable()", reach, tc.want)

			// Weighted and sampled strategies draw at random, so each request is
			// asked enough times for every draw to surface.
			var selected []string
			for range 20 {
				for _, req := range requests {
					keys, err := s.SelectTargets(req)
					if err != nil {
						t.Fatalf("SelectTargets(%+v): %v", req, err)
					}
					for _, key := range keys {
						if !slices.Contains(selected, key) {
							selected = append(selected, key)
						}
					}
				}
			}
			assertSameKeys(t, "keys SelectTargets returned", selected, tc.want)
		})
	}
}

func assertSameKeys(t *testing.T, what string, got, want []string) {
	t.Helper()
	g := slices.Clone(got)
	w := slices.Clone(want)
	slices.Sort(g)
	slices.Sort(w)
	if !slices.Equal(g, w) {
		t.Errorf("%s = %v, want %v", what, g, w)
	}
}
