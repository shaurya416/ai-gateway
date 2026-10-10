package bootstrap

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
	"github.com/ferro-labs/ai-gateway/pkg/logger"
	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestWarnUnroutableTargets proves a misconfigured target is audible at startup,
// and that the total case is separated from the partial one: every target
// unroutable means the instance serves nothing and is logged at error level,
// while some targets unroutable is a degraded instance that still serves and is
// logged as a warning. Neither exits — a target whose credential has not been
// rolled out yet starts working the moment the secret lands.
func TestWarnUnroutableTargets(t *testing.T) {
	tests := []struct {
		name       string
		targets    []string
		registered []string
		wantLevel  string
		wantNames  []string
		wantSilent bool
	}{
		{
			name:       "every target resolves logs nothing",
			targets:    []string{"a", "b"},
			registered: []string{"a", "b"},
			wantSilent: true,
		},
		{
			name:       "one unroutable target warns",
			targets:    []string{"a", "typo-provider"},
			registered: []string{"a"},
			wantLevel:  "WARN",
			wantNames:  []string{"typo-provider"},
		},
		{
			name:       "no routable target at all is an error",
			targets:    []string{"typo-provider"},
			registered: []string{"a"},
			wantLevel:  "ERROR",
			wantNames:  []string{"typo-provider"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			targets := make([]config.Target, 0, len(tt.targets))
			for _, name := range tt.targets {
				targets = append(targets, config.Target{VirtualKey: name})
			}
			gw, err := aigateway.New(config.Config{
				Strategy: config.StrategyConfig{Mode: config.ModeFallback},
				Targets:  targets,
			})
			if err != nil {
				t.Fatalf("New gateway: %v", err)
			}
			t.Cleanup(func() {
				if closeErr := gw.Close(); closeErr != nil {
					t.Errorf("close gateway: %v", closeErr)
				}
			})
			for _, name := range tt.registered {
				gw.RegisterProvider(stubProvider{name: name})
			}

			var buf bytes.Buffer
			previous := logger.Default()
			logger.SetDefault(logger.New(logger.Options{Level: "debug", Output: &buf}))
			t.Cleanup(func() { logger.SetDefault(previous) })

			warnUnroutableTargets(gw)

			got := buf.String()
			if tt.wantSilent {
				if strings.Contains(got, "target") {
					t.Fatalf("expected no target diagnostic, got: %s", got)
				}
				return
			}
			if !strings.Contains(got, `"level":"`+tt.wantLevel+`"`) {
				t.Errorf("log level %q missing from: %s", tt.wantLevel, got)
			}
			for _, name := range tt.wantNames {
				if !strings.Contains(got, name) {
					t.Errorf("target %q missing from log: %s", name, got)
				}
			}
		})
	}
}

// stubProvider is the minimum core.Provider needed to occupy a name in the
// gateway's registry.
type stubProvider struct{ name string }

func (s stubProvider) Name() string              { return s.name }
func (s stubProvider) SupportsModel(string) bool { return true }
func (s stubProvider) Models() []providers.ModelInfo {
	return nil
}
func (s stubProvider) Complete(context.Context, providers.Request) (*providers.Response, error) {
	return nil, nil
}

var _ core.Provider = stubProvider{}

// The startup report of unroutable targets has to describe the config the
// gateway runs. A config persisted in the store replaces the file's whole once
// the config manager adopts it, and the report ran before that, against the
// file: a stored config whose every target named an unregistered provider
// started with no error at all — every request then failing with a routing
// error — while a file the store had superseded logged one for a gateway that
// served.
func TestBuildServerReportsUnroutableTargetsOfTheActiveConfig(t *testing.T) {
	const totalFailure = "no configured target resolves to a registered provider"
	tests := []struct {
		name        string
		fileTarget  string
		storeTarget string
		wantError   bool
	}{
		{name: "stored config names an unregistered provider", fileTarget: "openai", storeTarget: "anthropic", wantError: true},
		{name: "superseded file names an unregistered provider", fileTarget: "anthropic", storeTarget: "openai", wantError: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			fileBody := "strategy:\n  mode: fallback\ntargets:\n  - virtual_key: " + tt.fileTarget + "\n"
			if err := os.WriteFile(configPath, []byte(fileBody), 0o600); err != nil {
				t.Fatal(err)
			}
			storeDSN := filepath.Join(dir, "config-store.db")
			store, err := repository.NewSQLiteConfigStore(t.Context(), storeDSN)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(t.Context(), config.Config{
				Strategy: config.StrategyConfig{Mode: config.ModeFallback},
				Targets:  []config.Target{{VirtualKey: tt.storeTarget}},
			}); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			t.Setenv("GATEWAY_CONFIG", configPath)
			t.Setenv("OPENAI_API_KEY", "test-key")
			t.Setenv("ANTHROPIC_API_KEY", "")
			t.Setenv("FERRO_MODEL_CATALOG_TIMEOUT", "0")
			t.Setenv("API_KEY_STORE_BACKEND", "memory")
			t.Setenv("CONFIG_STORE_BACKEND", "sqlite")
			t.Setenv("CONFIG_STORE_DSN", storeDSN)
			t.Setenv("REQUEST_LOG_STORE_BACKEND", "")
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
			t.Setenv("PORT", "0")

			var buf bytes.Buffer
			lg := logger.New(logger.Options{Level: "debug", Output: &buf})
			previous := logger.Default()
			logger.SetDefault(lg)
			t.Cleanup(func() { logger.SetDefault(previous) })

			app, err := buildServer(t.Context(), lg)
			if err != nil {
				t.Fatalf("buildServer: %v", err)
			}
			t.Cleanup(func() {
				if err := closeRuntimeResources(app.resources(), app.otelShutdown); err != nil {
					t.Errorf("close runtime resources: %v", err)
				}
			})
			if got := app.gw.GetConfig().Targets[0].VirtualKey; got != tt.storeTarget {
				t.Fatalf("active target = %q, want the stored config's %q", got, tt.storeTarget)
			}

			var report string
			for _, line := range strings.Split(buf.String(), "\n") {
				if strings.Contains(line, totalFailure) {
					report = line
				}
			}
			switch {
			case tt.wantError && report == "":
				t.Fatalf("no startup error for the active config, whose only target %q has no registered provider; log:\n%s",
					tt.storeTarget, buf.String())
			case tt.wantError && !strings.Contains(report, tt.storeTarget):
				t.Fatalf("startup error does not name the active config's target %q: %s", tt.storeTarget, report)
			case !tt.wantError && report != "":
				t.Fatalf("startup reported the superseded file's target as the gateway's, though the active config routes: %s", report)
			}
		})
	}
}
