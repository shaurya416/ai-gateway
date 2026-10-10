package repository

import (
	"path/filepath"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
)

// LoadHistory reads the newest window of the trail and nothing older, but the
// trail keeps every version. LoadVersion is how a version outside that window
// is reached, so it has to answer for one the window does not hold.
func TestSQLConfigStore_LoadVersionReachesPastTheHistoryWindow(t *testing.T) {
	ctx := t.Context()
	store, err := NewSQLiteConfigStore(ctx, filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("new config store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.Save(ctx, fallbackConfig()); err != nil {
		t.Fatalf("save version 1: %v", err)
	}
	for range maxConfigHistoryEntries {
		if err := store.Save(ctx, singleConfig()); err != nil {
			t.Fatalf("save later version: %v", err)
		}
	}
	if err := store.Save(WithRollbackFrom(ctx, 3), fallbackConfig()); err != nil {
		t.Fatalf("save rollback version: %v", err)
	}

	history, err := store.LoadHistory(ctx)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	if history[0].Version == 1 {
		t.Fatal("version 1 is inside the history window; the test needs it outside")
	}

	v1, ok, err := store.LoadVersion(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("LoadVersion(1) = ok %v, err %v; want the stored version", ok, err)
	}
	if v1.Version != 1 || v1.Config.Strategy.Mode != config.ModeFallback || len(v1.Config.Targets) != 2 {
		t.Fatalf("LoadVersion(1) = version %d mode %q with %d targets, want version 1 as saved",
			v1.Version, v1.Config.Strategy.Mode, len(v1.Config.Targets))
	}
	if v1.RolledBackFrom != nil {
		t.Fatalf("version 1 reports rolled_back_from = %d; it was an ordinary save", *v1.RolledBackFrom)
	}

	newest, ok, err := store.LoadVersion(ctx, maxConfigHistoryEntries+2)
	if err != nil || !ok {
		t.Fatalf("LoadVersion(newest) = ok %v, err %v", ok, err)
	}
	if newest.RolledBackFrom == nil || *newest.RolledBackFrom != 3 {
		t.Fatalf("newest rolled_back_from = %v, want 3", newest.RolledBackFrom)
	}

	if _, ok, err := store.LoadVersion(ctx, maxConfigHistoryEntries+3); err != nil || ok {
		t.Fatalf("LoadVersion(unrecorded) = ok %v, err %v; want not found and no error", ok, err)
	}
}
