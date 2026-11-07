package budget

import (
	"context"
	"fmt"
	"testing"

	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

// reportingPlugin is a budget at $1 a prompt token and a $1 cap in storeID,
// tracking at most maxKeys keys, whose limit reports are appended to reports.
func reportingPlugin(t *testing.T, storeID string, maxKeys int, reports *[]plugin.LimitExceeded) *Plugin {
	t.Helper()
	p := &Plugin{}
	p.SetLimitSink(func(le plugin.LimitExceeded) { *reports = append(*reports, le) })
	if err := p.Init(map[string]any{
		"store_id":           storeID,
		"spend_limit_usd":    1.0,
		"input_per_m_tokens": 1_000_000.0,
		"max_keys":           maxKeys,
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestBudget_RecordingAnExistingKeyAtTheCapEvictsNothing: eviction makes room
// for a NEW key. A request for a key already tracked adds to its spend; it
// must not evict the lowest spender, which may be that very key.
func TestBudget_RecordingAnExistingKeyAtTheCapEvictsNothing(t *testing.T) {
	const storeID = "test-existing-key-at-cap"
	defer ResetStore(storeID)
	p := makePlugin(t, maxKeysConfig(storeID, 2))

	spend(t, p, "key-a", 10)
	spend(t, p, "key-b", 20) // the store is now at its cap
	spend(t, p, "key-a", 5)

	if got := p.store.get("key-a"); got != 15 {
		t.Errorf("key-a spend = %v, want 15: a second request for a tracked key lost its earlier spend", got)
	}
	if got := p.store.get("key-b"); got != 20 {
		t.Errorf("key-b spend = %v, want 20: recording a tracked key evicted another", got)
	}
}

// TestBudget_MaxKeysZeroIsUnlimited: max_keys 0 tracks every key.
func TestBudget_MaxKeysZeroIsUnlimited(t *testing.T) {
	const storeID = "test-max-keys-zero"
	defer ResetStore(storeID)
	p := makePlugin(t, maxKeysConfig(storeID, 0))

	for i := 1; i <= 5; i++ {
		spend(t, p, fmt.Sprintf("key-%d", i), i)
	}
	for i := 1; i <= 5; i++ {
		if got := p.store.get(fmt.Sprintf("key-%d", i)); got != float64(i) {
			t.Errorf("key-%d spend = %v, want %d: max_keys 0 is no cap", i, got, i)
		}
	}
}

// TestBudget_ZeroCostRequestEvictsNothing: a request that costs nothing is not
// recorded, so it cannot take a tracked key's place. Otherwise a stream of
// requests the budget prices at $0 — image generation billed per image, say —
// from fresh keys evicts a spending key and restarts its cap at $0.
func TestBudget_ZeroCostRequestEvictsNothing(t *testing.T) {
	const storeID = "test-zero-cost-no-evict"
	defer ResetStore(storeID)
	p := makePlugin(t, maxKeysConfig(storeID, 1))

	spend(t, p, "spender", 500)
	spend(t, p, "free-rider", 0)

	if got := p.store.get("spender"); got != 500 {
		t.Errorf("spender spend = %v, want 500: a $0 request from another key evicted it", got)
	}
}

// TestBudget_UnlimitedNeverRefuses: spend_limit_usd 0 (the default) is no cap,
// whatever a key has spent.
func TestBudget_UnlimitedNeverRefuses(t *testing.T) {
	const storeID = "test-unlimited"
	defer ResetStore(storeID)
	p := makePlugin(t, map[string]any{
		"store_id":           storeID,
		"input_per_m_tokens": 1_000_000.0,
	})

	if check(t, p, "key") {
		t.Fatal("a budget with no spend_limit_usd refused a key that has spent nothing")
	}
	spend(t, p, "key", 1000)
	if check(t, p, "key") {
		t.Fatal("a budget with no spend_limit_usd refused a key after it spent")
	}
}

// TestBudget_EmptyAPIKeyIsNotTracked: a request carrying an empty key is a
// request without a key, so it is neither billed nor refused. Tracking "" as a
// key would pool every such request into one shared budget.
func TestBudget_EmptyAPIKeyIsNotTracked(t *testing.T) {
	const storeID = "test-empty-key"
	defer ResetStore(storeID)
	p := makePlugin(t, map[string]any{
		"store_id":           storeID,
		"spend_limit_usd":    1.0,
		"input_per_m_tokens": 1_000_000.0,
	})

	spend(t, p, "", 5)
	if got := p.store.get(""); got != 0 {
		t.Errorf("an empty api_key was billed $%v", got)
	}
	if check(t, p, "") {
		t.Error("an empty api_key was refused")
	}
}

// TestBudget_CacheOnlyRatesAreEnforceable: the load check refuses a limit no
// rate can reach, and a cache rate is a rate. A budget priced on cached tokens
// alone loads and bills.
func TestBudget_CacheOnlyRatesAreEnforceable(t *testing.T) {
	for _, rateKey := range []string{"cache_read_per_m_tokens", "cache_write_per_m_tokens"} {
		t.Run(rateKey, func(t *testing.T) {
			storeID := "test-cache-only-" + rateKey
			defer ResetStore(storeID)
			p := makePlugin(t, map[string]any{
				"store_id":        storeID,
				"spend_limit_usd": 1.0,
				rateKey:           1_000_000.0,
			})

			pctx := pctxWithKey("key")
			pctx.Stage = plugin.StageAfterRequest
			pctx.Response = &providers.Response{Usage: providers.Usage{
				PromptTokens: 2, CacheReadTokens: 2, CacheWriteTokens: 2,
			}}
			if err := p.Execute(context.Background(), pctx); err != nil {
				t.Fatal(err)
			}
			plugin.PutContext(pctx)

			if got := p.store.get("key"); got != 2 {
				t.Fatalf("spend = %v, want 2 at the configured %s", got, rateKey)
			}
			if !check(t, p, "key") {
				t.Fatal("a key over its cap was admitted")
			}
		})
	}
}

// TestBudget_ResetAloneRearmsTheLimitReport: a reset starts a new run of
// refusals even when no admitted check follows it — the next request to
// cross the cap is reported, as AGENTS.md's budget.exceeded row promises.
func TestBudget_ResetAloneRearmsTheLimitReport(t *testing.T) {
	for name, reset := range map[string]func(storeID string){
		"ResetStoreKey": func(storeID string) { ResetStoreKey(storeID, "key-a") },
		"ResetStore":    ResetStore,
	} {
		t.Run(name, func(t *testing.T) {
			storeID := "test-reset-rearms-" + name
			defer ResetStore(storeID)
			var reports []plugin.LimitExceeded
			p := reportingPlugin(t, storeID, 0, &reports)

			spend(t, p, "key-a", 2)
			check(t, p, "key-a")
			reset(storeID)
			// An admitted request that completes after the reset.
			spend(t, p, "key-a", 3)
			if !check(t, p, "key-a") {
				t.Fatal("a key over its cap was admitted")
			}
			if len(reports) != 2 {
				t.Fatalf("got %d limit reports, want 2: the refusal after a reset is a new run", len(reports))
			}
		})
	}
}

// TestBudget_AdmissionRearmsTheLimitReport: an admitted check ends a run, so
// the key's next refusal is reported. Here a reload raises the cap over a
// shared store, admitting the key, and a later reload lowers it again.
func TestBudget_AdmissionRearmsTheLimitReport(t *testing.T) {
	const storeID = "test-admission-rearms"
	defer ResetStore(storeID)
	var reports []plugin.LimitExceeded
	low := reportingPlugin(t, storeID, 0, &reports)

	spend(t, low, "key-a", 2)
	if !check(t, low, "key-a") {
		t.Fatal("a key over its cap was admitted")
	}

	raised := makePlugin(t, map[string]any{
		"store_id":           storeID,
		"spend_limit_usd":    10.0,
		"input_per_m_tokens": 1_000_000.0,
	})
	if check(t, raised, "key-a") {
		t.Fatal("a key under its raised cap was refused")
	}

	lowered := reportingPlugin(t, storeID, 0, &reports)
	if !check(t, lowered, "key-a") {
		t.Fatal("a key over its lowered cap was admitted")
	}
	if len(reports) != 2 {
		t.Fatalf("got %d limit reports, want 2: the admitted check ended the first run", len(reports))
	}
}

// TestBudget_EvictionRearmsTheLimitReport: an evicted key restarts at $0, so
// when it crosses its cap again that is a new run of refusals and is reported
// — on the single eviction at the cap, and on the bulk eviction a reload that
// lowers max_keys triggers.
func TestBudget_EvictionRearmsTheLimitReport(t *testing.T) {
	t.Run("at the cap", func(t *testing.T) {
		const storeID = "test-evict-rearms-single"
		defer ResetStore(storeID)
		var reports []plugin.LimitExceeded
		p := reportingPlugin(t, storeID, 1, &reports)

		spend(t, p, "key-a", 2)
		check(t, p, "key-a")
		spend(t, p, "key-b", 1) // evicts key-a
		if got := p.store.get("key-a"); got != 0 {
			t.Fatalf("key-a spend = %v, want it evicted", got)
		}
		spend(t, p, "key-a", 3)
		if !check(t, p, "key-a") {
			t.Fatal("a key over its cap was admitted")
		}
		if len(reports) != 2 {
			t.Fatalf("got %d limit reports, want 2: an evicted key's next refusal is a new run", len(reports))
		}
	})

	t.Run("max_keys lowered on reload", func(t *testing.T) {
		const storeID = "test-evict-rearms-bulk"
		defer ResetStore(storeID)
		var reports []plugin.LimitExceeded
		before := reportingPlugin(t, storeID, 10, &reports)

		spend(t, before, "key-a", 2)
		check(t, before, "key-a")
		spend(t, before, "key-b", 5)
		spend(t, before, "key-c", 6)

		after := reportingPlugin(t, storeID, 1, &reports)
		spend(t, after, "key-d", 7) // evicts key-a, key-b and key-c in one pass
		if got := after.store.get("key-a"); got != 0 {
			t.Fatalf("key-a spend = %v, want it evicted", got)
		}
		spend(t, after, "key-a", 3)
		if !check(t, after, "key-a") {
			t.Fatal("a key over its cap was admitted")
		}
		if len(reports) != 2 {
			t.Fatalf("got %d limit reports, want 2: an evicted key's next refusal is a new run", len(reports))
		}
	})
}

// TestBudget_CacheServedResponseIsNotBilled: SkipProvider at after_request
// means the response came from the cache and no provider was called, so the
// usage it carries is not spend.
func TestBudget_CacheServedResponseIsNotBilled(t *testing.T) {
	const storeID = "test-cache-served"
	defer ResetStore(storeID)
	p := makePlugin(t, map[string]any{
		"store_id":           storeID,
		"spend_limit_usd":    1.0,
		"input_per_m_tokens": 1_000_000.0,
	})

	pctx := pctxWithKey("key")
	pctx.Stage = plugin.StageAfterRequest
	pctx.SkipProvider = true
	pctx.Response = &providers.Response{Usage: providers.Usage{PromptTokens: 5}}
	if err := p.Execute(context.Background(), pctx); err != nil {
		t.Fatal(err)
	}
	plugin.PutContext(pctx)

	if got := p.store.get("key"); got != 0 {
		t.Fatalf("a cache-served response was billed $%v", got)
	}
}
