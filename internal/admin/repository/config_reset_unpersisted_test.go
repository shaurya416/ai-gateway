package repository

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	// The failed-revert case needs a plugin the gateway can build once and not
	// again; word-filter resolves its ${VAR} references when it is built.
	_ "github.com/ferro-labs/ai-gateway/plugin/wordfilter"
)

// TestGatewayConfigManager_ResetConfig_UnpersistedResetLeavesNoDivergence pins
// what a reset the store cannot take leaves running.
//
// A reset applies the startup config first and then clears the persisted
// override. When the clear failed and recording the startup config in its place
// failed too, the gateway was left on the startup config while the store still
// held the override: the caller was answered "failed to reset config", the
// instance stayed ready, and the next start adopted the override back — the
// divergence ReloadConfig reverts to avoid. A request whose client hangs up is
// enough: both writes run on its context, and both are refused.
func TestGatewayConfigManager_ResetConfig_UnpersistedResetLeavesNoDivergence(t *testing.T) {
	ctx := context.Background()
	gw, err := newTestGateway(t, singleConfig())
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	store, err := NewSQLiteConfigStore(ctx, filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("new config store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mgr, err := NewGatewayConfigManager(gw, store)
	if err != nil {
		t.Fatalf("new config manager: %v", err)
	}

	if err := mgr.ReloadConfig(ctx, fallbackConfig()); err != nil {
		t.Fatalf("apply override: %v", err)
	}

	hungUp, cancel := context.WithCancel(ctx)
	cancel()
	err = mgr.ResetConfig(hungUp)
	if err == nil {
		t.Fatal("a reset neither write could record was reported as a success")
	}
	if !errors.Is(err, ErrConfigPersistence) {
		t.Fatalf("expected a persistence-classified error, got: %v", err)
	}

	stored, ok, err := store.Load(ctx)
	if err != nil || !ok {
		t.Fatalf("load stored config: ok=%v err=%v", ok, err)
	}
	if stored.Strategy.Mode != config.ModeFallback {
		t.Fatalf("this test needs the override to survive in the store; it holds %q", stored.Strategy.Mode)
	}
	if running := mgr.GetConfig(); running.Strategy.Mode != stored.Strategy.Mode {
		t.Fatalf("the gateway runs %q while the store holds %q, and the next start adopts the store's: a reset that could not be recorded must leave the override running",
			running.Strategy.Mode, stored.Strategy.Mode)
	}
	if err := mgr.Ping(ctx); err != nil {
		t.Fatalf("the reverted manager should report ready: %v", err)
	}
}

// The revert can fail too, when the override can no longer be rebuilt. The
// gateway then runs the startup config while the store holds the override, and
// nothing in the process can decide which is right, so the instance stops
// reporting ready — exactly as ReloadConfig's own double failure does.
func TestGatewayConfigManager_ResetConfig_FailedRevertStopsReportingReady(t *testing.T) {
	ctx := context.Background()
	gw, err := newTestGateway(t, singleConfig())
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	store, err := NewSQLiteConfigStore(ctx, filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("new config store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mgr, err := NewGatewayConfigManager(gw, store)
	if err != nil {
		t.Fatalf("new config manager: %v", err)
	}

	// ${VAR} is resolved when the plugin is constructed, so the variable going
	// away is what makes re-applying the override fail.
	t.Setenv("FERRO_TEST_RESET_BLOCKED_WORD", "hunter2")
	override := fallbackConfig()
	override.Plugins = []config.PluginConfig{{
		Name:    "word-filter",
		Type:    "guardrail",
		Stage:   "before_request",
		Enabled: true,
		Config:  map[string]any{"blocked_words": []any{"${FERRO_TEST_RESET_BLOCKED_WORD}"}},
	}}
	if err := mgr.ReloadConfig(ctx, override); err != nil {
		t.Fatalf("apply override: %v", err)
	}
	if err := os.Unsetenv("FERRO_TEST_RESET_BLOCKED_WORD"); err != nil {
		t.Fatalf("unset env: %v", err)
	}

	hungUp, cancel := context.WithCancel(ctx)
	cancel()
	err = mgr.ResetConfig(hungUp)
	if !errors.Is(err, ErrConfigPersistence) {
		t.Fatalf("expected a persistence-classified error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "revert applied config failed") {
		t.Fatalf("this test needs the revert to fail; it did not: %v", err)
	}
	if pingErr := mgr.Ping(ctx); pingErr == nil {
		t.Fatal("the running config and the store disagree and the instance still reports ready; a restart would silently adopt the store's config")
	}
}
