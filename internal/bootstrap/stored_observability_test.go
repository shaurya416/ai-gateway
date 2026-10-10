package bootstrap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
	"github.com/ferro-labs/ai-gateway/observability"
	"github.com/ferro-labs/ai-gateway/pkg/logger"
)

// storedExporterSequence keeps registered exporter names unique, since the
// registry refuses a name twice and a test may run more than once per process.
var storedExporterSequence atomic.Int32

// countingExporter records how many times the startup pipeline initialised it.
type countingExporter struct {
	name  string
	inits *atomic.Int32
}

func (e countingExporter) Name() string { return e.name }
func (e countingExporter) Init(context.Context, map[string]any) error {
	e.inits.Add(1)
	return nil
}
func (countingExporter) Export(context.Context, observability.Event) error { return nil }
func (countingExporter) Shutdown(context.Context) error                    { return nil }

// A config persisted through the admin API supersedes the file as a whole —
// observability included. The admin API accepts an observability change with a
// warning that it "takes effect on the next restart", so the next start must
// build the tracing pipeline from the stored section. It was built from the
// file's instead, ahead of the store being read: an exporter enabled through
// the admin API never started, on that restart or any later one, while
// GET /admin/config reported it enabled.
func TestBuildServerBuildsObservabilityFromTheStoredConfig(t *testing.T) {
	var inits atomic.Int32
	exporterName := fmt.Sprintf("stored-config-exporter-%d", storedExporterSequence.Add(1))
	observability.RegisterExporter(exporterName, func() observability.Exporter {
		return countingExporter{name: exporterName, inits: &inits}
	})

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("strategy:\n  mode: single\ntargets:\n  - virtual_key: openai\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	storeDSN := filepath.Join(dir, "config-store.db")

	// What PUT /admin/config would have persisted: the file's routing plus an
	// exporter the file does not name.
	store, err := repository.NewSQLiteConfigStore(t.Context(), storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	stored := config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "openai"}},
		Observability: config.ObservabilityConfig{
			Exporters: []config.ExporterConfig{{Name: exporterName, Enabled: true}},
		},
	}
	if err := store.Save(t.Context(), stored); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GATEWAY_CONFIG", configPath)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("FERRO_MODEL_CATALOG_TIMEOUT", "0")
	t.Setenv("API_KEY_STORE_BACKEND", "memory")
	t.Setenv("CONFIG_STORE_BACKEND", "sqlite")
	t.Setenv("CONFIG_STORE_DSN", storeDSN)
	t.Setenv("REQUEST_LOG_STORE_BACKEND", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("PORT", "0")

	app, err := buildServer(t.Context(), logger.New(logger.FromEnv()))
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := closeRuntimeResources(app.resources(), app.otelShutdown); err != nil {
			t.Errorf("close runtime resources: %v", err)
		}
	})

	active := app.gw.GetConfig()
	if len(active.Observability.Exporters) != 1 {
		t.Fatalf("the stored config was not adopted: active exporters = %+v", active.Observability.Exporters)
	}
	if got := inits.Load(); got != 1 {
		t.Fatalf("the exporter the active config enables was initialised %d times, want 1: "+
			"the tracing pipeline was built from the file rather than the config the gateway runs", got)
	}
	if app.gw.Observability() == observability.NoOp() {
		t.Fatal("the gateway runs the NoOp observability provider although its active config enables an exporter")
	}
}
