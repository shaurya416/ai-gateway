package aigateway

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/latency"
	"github.com/ferro-labs/ai-gateway/internal/strategies"
	"github.com/ferro-labs/ai-gateway/models"
	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

// Strategy construction from Gateway config.

// getStrategy lazily builds the chat strategy from config and registered
// providers. Circuit breakers are built once and applied in the provider
// lookup closure.
func (g *Gateway) getStrategy() (strategies.Strategy, error) {
	return g.strategyFor("")
}

// surfaceStream keys the streaming chat strategy. It is not a surface name
// operators see — a stream is chat, and is labelled as chat — only the key
// under which strategyFor builds chat's strategy over the targets that can
// stream.
const surfaceStream = "chat.stream"

// strategyFor is getStrategy for one surface. It is the same strategy built
// from the same config, and differs in two places only. The provider lookup
// drops a target whose provider cannot serve this surface: that keeps a
// chat-only target from winning a weighted draw it can never serve — which
// would then hand its whole share to whichever capable target follows it in
// the rotation — or from taking the cheapest or fastest slot under a ranking
// mode. And a rule the surface's requests cannot be asked is left out: a
// conditional rule on a chat-only predicate drops out of every other surface's
// rule list (see withoutChatOnlyConditions), and content-based rules, which
// read a prompt those requests do not have, are replaced by the no-match
// answer (see firstServingTarget). Model eligibility, order, and the tail are
// then decided once, in internal/strategies, for chat and every other surface.
//
// Streaming takes the first difference and not the second: its lookup holds
// only the targets that can stream, and its rules are chat's, `stream` and
// `has_tools` included, and so are its content rules (surfaceStream).
func (g *Gateway) strategyFor(surface string) (strategies.Strategy, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.strategyForLocked(surface)
}

// governedStrategy returns the strategy a request on surface is routed by and
// the plugin manager it is governed by, acquired for the request, read under
// one hold of the lock so both belong to one config.
//
// A request runs its before_request stage first and is routed afterwards, but
// what it is routed by has to be decided when its plugins are: a reload
// installs a config's plugins and its targets together, and a request handed
// the plugins of one and the strategy of the next reached the new config's
// targets having been screened by none of its guardrails. The strategy is
// still asked for an order only after the stage, on the request the stage
// settled on.
//
// The returned release must be called once the request has finished with the
// plugins, whether or not err is nil.
func (g *Gateway) governedStrategy(surface string) (strategies.Strategy, *plugin.Manager, func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.governedStrategyLocked(surface)
}

// governedStrategyLocked is governedStrategy for a caller holding g.mu for
// writing, so it can take the rest of its request snapshot in the same hold.
func (g *Gateway) governedStrategyLocked(surface string) (strategies.Strategy, *plugin.Manager, func(), error) {
	plugins := g.plugins
	release := acquirePluginManager(plugins)
	s, err := g.strategyForLocked(surface)
	return s, plugins, release, err
}

// strategyForLocked is strategyFor for a caller holding g.mu for writing.
func (g *Gateway) strategyForLocked(surface string) (strategies.Strategy, error) {
	if s, ok := g.strategies[surface]; ok {
		return s, nil
	}

	g.ensureCircuitBreakersLocked()
	g.ensureProviderLimitersLocked()

	// Snapshot every map the lookup closure reads, under the write lock already
	// held. The closure runs inside Strategy.SelectTargets with no lock held, so
	// capturing local copies here is the only safe access pattern.
	// maps.Clone is a shallow copy — safe because map values (Provider, *CB) are
	// themselves immutable references; we never mutate through them in the closure.
	cbSnap := maps.Clone(g.circuitBreakers)
	limSnap := maps.Clone(g.limiters)

	// The provider snapshot holds each provider under the routing-index view of
	// its model set: every strategy selects a target on SupportsModel, and that
	// answer must cover the same models FindByModel admits and /v1/models
	// advertises — a provider's own SupportsModel is often much narrower.
	// Building the views here rather than per lookup keeps the request path
	// allocation-free, and the snapshot is rebuilt whenever the index is,
	// because both are replaced under the same lock that clears g.strategies.
	providerSnap := make(map[string]providers.Provider, len(g.providers))
	for name, p := range g.providers {
		if surface != "" && !providerSupportsSurface(p, surface) {
			continue
		}
		providerSnap[name] = withIndexedModels(name, p, g.modelIndex.targetOwners)
	}

	// Provider lookup with transparent circuit-breaker and concurrency-limit
	// decoration.
	//
	// The closure is captured into the strategy and invoked later from the
	// request hot path, AFTER Route/RouteStream have released g.mu. It reads
	// from the snapshots captured above, so no lock is needed in the closure.
	lookup := func(name string) (providers.Provider, bool) {
		p, ok := providerSnap[name]
		if !ok {
			return nil, false
		}
		return decorateProvider(name, p, cbSnap[name], limSnap[name]), true
	}

	targets := make([]strategies.Target, len(g.config.Targets))
	for i, t := range g.config.Targets {
		targets[i] = strategies.Target{
			VirtualKey: t.VirtualKey,
			Weight:     t.Weight,
			ModelMap:   t.ModelMap,
		}
	}

	strategyCfg := g.config.Strategy
	promptless := surface != "" && surface != surfaceStream
	if promptless {
		strategyCfg.Conditions = withoutChatOnlyConditions(strategyCfg.Conditions)
	}

	var s strategies.Strategy
	if promptless && strategyCfg.Mode == config.ModeContentBased && len(targets) > 0 {
		// Content rules read the prompt, and a request on this surface has
		// none, so it takes the no-match answer (see firstServingTarget).
		// Decided here, from the config this strategy is built from, rather
		// than beside it: a check made under a separate hold of the lock let
		// a reload into content-based land between the two, and the request
		// was then routed by the rules it must never be asked.
		s = firstServingTarget{targets: targets, lookup: lookup}
	} else {
		var err error
		s, err = buildStrategy(strategyCfg, targets, lookup, g.latencyTracker, g.catalog)
		if err != nil {
			return nil, err
		}
	}
	built := &builtStrategy{Strategy: s, walk: walkSettingsFor(g.config.Strategy)}
	if g.strategies == nil {
		g.strategies = make(map[string]strategies.Strategy)
	}
	g.strategies[surface] = built
	return built, nil
}

// builtStrategy is a strategy together with the settings of the strategy block
// it was built from that decide how its order is walked (see planFor).
//
// They travel with the strategy because a request holds its strategy from the
// moment it starts to the moment it is walked — across the before_request
// stage, and across every turn of an MCP tool loop — and a reload can replace
// the config in between. Read from the live config at the walk instead, a
// request was handed one config's target order and another config's rule for
// walking it: an order from fallback over [a, b], walked under a reloaded
// single's "attempt the head only", attempted a target the new config no
// longer lists and answered 404 for a model both configs serve.
type builtStrategy struct {
	strategies.Strategy
	walk walkSettings
}

// walkSettings is what planFor reads from a strategy block.
type walkSettings struct {
	advance    bool
	failoverOn []int
	// abLabels maps a variant's target to its label under ab-test, the first
	// variant naming a target winning. Nil under every other mode.
	abLabels map[string]string
}

// walkSettingsFor resolves the walk settings of one strategy block. The slice
// and map it returns are its own, so a later change to cfg cannot reach a
// strategy already built from it.
func walkSettingsFor(cfg config.StrategyConfig) walkSettings {
	w := walkSettings{
		advance:    advancesPastFailure(cfg.Mode),
		failoverOn: slices.Clone(cfg.FailoverOnStatusCodes),
	}
	if cfg.Mode == config.ModeABTest {
		w.abLabels = make(map[string]string, len(cfg.ABVariants))
		for _, variant := range cfg.ABVariants {
			if _, seen := w.abLabels[variant.TargetKey]; !seen {
				w.abLabels[variant.TargetKey] = variant.Label
			}
		}
	}
	return w
}

// buildStrategy maps a validated StrategyConfig onto a strategy.
//
// Every error it can return is one config.ValidateConfig has already rejected,
// which is the point: strategies are built LAZILY, on the first request that
// needs one, so an error here would be a 500 on a request no provider was ever
// asked to serve. Construction stays lazy because it must — the provider lookup
// snapshots the registry, and providers are registered after New returns — so
// what makes that safe is validation being total over these failures rather than
// construction moving earlier. TestValidateConfigCoversStrategyConstruction
// holds the two in step.
func buildStrategy(
	cfg config.StrategyConfig,
	targets []strategies.Target,
	lookup strategies.ProviderLookup,
	tracker *latency.Tracker,
	catalog models.Catalog,
) (strategies.Strategy, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("no targets configured for %s strategy", cfg.Mode)
	}
	switch cfg.Mode {
	case config.ModeSingle, "":
		return strategies.NewSingle(targets[0]), nil
	case config.ModeFallback:
		return strategies.NewFallback(targets), nil
	case config.ModeLoadBalance:
		return strategies.NewLoadBalance(targets, lookup).WithSticky(stickyFrom(cfg.Sticky)), nil
	case config.ModeLatency:
		return strategies.NewLeastLatency(targets, lookup, tracker), nil
	case config.ModeCostOptimized:
		return strategies.NewCostOptimized(targets, lookup, catalog, cfg.UnpricedStrategy), nil
	case config.ModeConditional:
		rules := make([]strategies.ConditionRule, 0, len(cfg.Conditions))
		for _, cond := range cfg.Conditions {
			rules = append(rules, strategies.ConditionRule{
				Key:     cond.Key,
				Value:   cond.Value,
				Field:   cond.Field,
				Targets: chainTargets(cond.Chain()),
			})
		}
		return strategies.NewConditional(rules, targets[0]), nil
	case config.ModeContentBased:
		rules := make([]strategies.ContentRule, 0, len(cfg.ContentConditions))
		for _, cc := range cfg.ContentConditions {
			rules = append(rules, strategies.ContentRule{
				Type:    strategies.ContentConditionType(cc.Type),
				Value:   cc.Value,
				Targets: chainTargets(cc.Chain()),
			})
		}
		return strategies.NewContentBased(rules, targets[0])
	case config.ModeABTest:
		variants := make([]strategies.ABTestVariant, 0, len(cfg.ABVariants))
		for _, v := range cfg.ABVariants {
			variants = append(variants, strategies.ABTestVariant{
				Target: strategies.Target{VirtualKey: v.TargetKey},
				Weight: v.Weight,
				Label:  v.Label,
			})
		}
		abt, err := strategies.NewABTest(variants, lookup)
		if err != nil {
			return nil, err
		}
		return abt.WithRoutingTargets(targets).WithSticky(stickyFrom(cfg.Sticky)), nil
	default:
		return nil, fmt.Errorf("unknown strategy mode: %s", cfg.Mode)
	}
}

// stickyFrom maps strategy.sticky onto the strategies' value. The TTL was
// validated at load, so a parse failure here reads as no TTL.
func stickyFrom(cfg *config.StickyConfig) strategies.Sticky {
	if cfg == nil {
		return strategies.Sticky{}
	}
	ttl, _ := time.ParseDuration(cfg.TTL)
	return strategies.Sticky{On: cfg.On, TTL: ttl}
}

// firstServingTarget is content-based routing for a surface whose requests
// carry no prompt — embeddings, images, rerank, moderation and audio.
//
// Content rules only look at a chat prompt. prompt_contains and prompt_regex
// find no match against an empty one, but prompt_not_contains is the negation
// of "some user message contains", which is vacuously true over no messages:
// asked of one of these requests it matched every one and won routing outright,
// whatever the rule said. No content rule can be evaluated without a prompt, so
// such a request takes the no-match answer: one target, the first configured
// one that is a candidate for its model on this surface — a rule list written
// for chat may well lead with a chat-only target — and the first target when
// none is, so the walk reports the same 404 it would for any other unroutable
// request.
//
// lookup is strategyFor's, which holds only the providers that can serve the
// surface and answers SupportsModel from the routing index, so a candidate
// here is exactly one the pipeline's candidateLocked admits.
type firstServingTarget struct {
	targets []strategies.Target
	lookup  strategies.ProviderLookup
}

func (s firstServingTarget) SelectTargets(req providers.Request) ([]string, error) {
	for _, t := range s.targets {
		if p, ok := s.lookup(t.VirtualKey); ok && p.SupportsModel(req.Model) {
			return []string{t.VirtualKey}, nil
		}
	}
	return []string{s.targets[0].VirtualKey}, nil
}

// withoutChatOnlyConditions returns conds less the rules on `stream` and
// `has_tools`, in order, for a surface other than chat.
//
// Both predicates describe a chat completion — whether it streams, whether it
// carries tools — and a request on any other surface has neither. Asked of
// one anyway, the matcher reads the field's zero value, so a "false" rule
// written for chat traffic matched every embeddings, image, rerank,
// moderation and audio request, ahead of the rule that names its model, and
// sent it to a target the operator chose for chat: a different provider than
// the one configured for that model, or a 404 from one that cannot serve the
// surface. Dropping the rule is the reading the strategies README gives these
// keys — chat-only — and the one content-based already gets on these
// surfaces (see firstServingTarget).
func withoutChatOnlyConditions(conds []config.Condition) []config.Condition {
	kept := make([]config.Condition, 0, len(conds))
	for _, c := range conds {
		if c.Key == config.ConditionKeyStream || c.Key == config.ConditionKeyHasTools {
			continue
		}
		kept = append(kept, c)
	}
	return kept
}

// chainTargets maps a rule's target chain onto strategy targets.
func chainTargets(keys []string) []strategies.Target {
	chain := make([]strategies.Target, len(keys))
	for i, key := range keys {
		chain[i] = strategies.Target{VirtualKey: key}
	}
	return chain
}
