package budget

import (
	"fmt"
	"testing"
)

// maxKeysConfig is a budget at $1 a prompt token, so a key's recorded spend is
// its token count, capped at maxKeys tracked keys in storeID.
func maxKeysConfig(storeID string, maxKeys int) map[string]any {
	return map[string]any{
		"store_id":           storeID,
		"spend_limit_usd":    1000.0,
		"input_per_m_tokens": 1_000_000.0,
		"max_keys":           maxKeys,
	}
}

// TestBudget_MaxKeysRaisedOnReloadStopsEviction covers the remedy the package
// documentation gives for keys losing their spend to eviction: raise max_keys.
// Spend lives in a process-level store that outlives a config reload, and the
// store kept the cap of the first instance that opened it, so a reload raising
// max_keys changed nothing — every key past the old cap still evicted another,
// and the evicted key's cap restarted at $0.
func TestBudget_MaxKeysRaisedOnReloadStopsEviction(t *testing.T) {
	const storeID = "test-max-keys-raised"
	defer ResetStore(storeID)

	before := makePlugin(t, maxKeysConfig(storeID, 2))
	spend(t, before, "key-a", 10)
	spend(t, before, "key-b", 20)

	// The reload: a new instance of the same store, with room for more keys.
	after := makePlugin(t, maxKeysConfig(storeID, 10))
	spend(t, after, "key-c", 30)

	for key, want := range map[string]float64{"key-a": 10, "key-b": 20, "key-c": 30} {
		if got := after.store.get(key); got != want {
			t.Errorf("%s spend = %v, want %v: max_keys was raised to 10 and only three keys are tracked", key, got, want)
		}
	}
}

// TestBudget_MaxKeysLoweredOnReloadBoundsTheStore is the other direction: a
// reload lowering max_keys bounds the store from the next key it tracks, rather
// than leaving it at the size the old cap allowed.
func TestBudget_MaxKeysLoweredOnReloadBoundsTheStore(t *testing.T) {
	const storeID = "test-max-keys-lowered"
	defer ResetStore(storeID)

	before := makePlugin(t, maxKeysConfig(storeID, 10))
	for i := 1; i <= 5; i++ {
		spend(t, before, fmt.Sprintf("key-%d", i), i*10)
	}

	after := makePlugin(t, maxKeysConfig(storeID, 2))
	spend(t, after, "key-new", 100)

	tracked := 0
	for i := 1; i <= 5; i++ {
		if after.store.get(fmt.Sprintf("key-%d", i)) != 0 {
			tracked++
		}
	}
	if after.store.get("key-new") != 100 {
		t.Error("the key just recorded is not tracked")
	} else {
		tracked++
	}
	if tracked > 2 {
		t.Errorf("%d keys tracked after a reload lowered max_keys to 2", tracked)
	}
	// The keys kept are the highest spenders, exactly as one eviction at the
	// cap keeps them.
	if after.store.get("key-5") != 50 {
		t.Error("the highest-spend key was evicted ahead of lower ones")
	}
}
