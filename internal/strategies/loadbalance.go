package strategies

import (
	"slices"

	"github.com/ferro-labs/ai-gateway/providers"
)

// LoadBalance distributes requests across targets using weighted random selection.
type LoadBalance struct {
	targets []Target
	lookup  ProviderLookup
	sticky  Sticky
}

// WithSticky pins each request's start target to a hash of its sticky key
// (see Sticky). Returns the receiver so callers can chain it.
func (lb *LoadBalance) WithSticky(s Sticky) *LoadBalance {
	lb.sticky = s
	return lb
}

// NewLoadBalance creates a new load balance strategy.
func NewLoadBalance(targets []Target, lookup ProviderLookup) *LoadBalance {
	return &LoadBalance{
		targets: targets,
		lookup:  lookup,
	}
}

// compatibleTargets returns the subset of lb.targets whose provider is
// registered and supports model, preserving declared order.
func (lb *LoadBalance) compatibleTargets(model string) []Target {
	var compatible []Target
	for _, t := range lb.targets {
		p, ok := lb.lookup(t.VirtualKey)
		if ok && p.SupportsModel(model) {
			compatible = append(compatible, t)
		}
	}
	return compatible
}

// SelectTargets returns the model-compatible, positively weighted targets
// rotated from a weight-biased start index, so the first attempted target is
// chosen by weight while the remainder stay available as fallbacks. Targets
// whose provider is missing or does not support the model are excluded, and so
// are drained ones.
//
// A target weighted 0 is excluded from the whole list, not only from the start
// index. The pipeline advances through this list when a target fails, and skips
// a target whose circuit is open or that is parked after a 429, so a drained
// target left in the tail took every request its sibling could not serve — the
// failover traffic arriving exactly while the operator was revoking its
// credential.
//
// It returns nil when no compatible target carries a positive weight. That is
// not the same as "no compatible target": it means the only targets that could
// serve this model have been drained to zero, and answering it from a drained
// target would undo the drain at exactly the moment the operator is relying on
// it — the credential is about to be revoked. A drained target is as unavailable
// as a missing one, so the caller reports the same 404.
func (lb *LoadBalance) SelectTargets(req providers.Request) ([]string, error) {
	weighted := lb.compatibleTargets(req.Model)
	weighted = slices.DeleteFunc(weighted, func(t Target) bool { return positiveWeight(t.Weight) == 0 })
	// Dropping the zero weights moves no positively weighted target's share of
	// the draw, so a sticky key keeps the target it was pinned to.
	startIdx, ok := weightedStartIndexAt(weighted, lb.sticky.unit(req.User))
	if !ok {
		return nil, nil
	}
	keys := make([]string, 0, len(weighted))
	for i := 0; i < len(weighted); i++ {
		keys = append(keys, weighted[(startIdx+i)%len(weighted)].VirtualKey)
	}
	return keys, nil
}
