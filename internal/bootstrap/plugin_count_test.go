package bootstrap

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/pkg/logger"
	"github.com/ferro-labs/ai-gateway/plugin"
	"github.com/ferro-labs/ai-gateway/providers"
)

// countProbePlugin is a plugin that does nothing, registered so a startup test
// can configure an enabled entry without depending on a built-in's config.
const countProbePlugin = "startup-count-probe"

type countProbe struct{}

func (countProbe) Name() string                                   { return countProbePlugin }
func (countProbe) Type() plugin.PluginType                        { return plugin.TypeLogging }
func (countProbe) Init(map[string]any) error                      { return nil }
func (countProbe) Execute(context.Context, *plugin.Context) error { return nil }
func (countProbe) Close() error                                   { return nil }

func init() {
	plugin.RegisterFactory(countProbePlugin, func() plugin.Plugin { return countProbe{} })
}

// pluginCountConfig has one enabled plugin entry and two disabled ones.
func pluginCountConfig() config.Config {
	return config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeFallback},
		Targets:  []config.Target{{VirtualKey: "openai"}},
		Plugins: []config.PluginConfig{
			{Name: countProbePlugin, Type: "logging", Stage: "before_request", Enabled: true},
			{Name: "word-filter", Type: "guardrail", Stage: "before_request", Enabled: false},
			{Name: "pii-redact", Type: "guardrail", Stage: "before_request", Enabled: false},
		},
	}
}

// A disabled entry is config, not a running plugin: the gateway builds nothing
// for it. Counting entries, the startup log reported "plugins loaded count=3"
// and the banner "3 plugins" for a gateway running one, so two guardrails
// read as in force that were not.
func TestStartupReportsCountOnlyEnabledPlugins(t *testing.T) {
	t.Run("plugins loaded log line", func(t *testing.T) {
		buf := captureDefaultLogger(t)
		cfg := pluginCountConfig()

		gw, err := BuildGateway(t.Context(), &cfg, providers.NewRegistry(), nil, logger.Default())
		if err != nil {
			t.Fatalf("BuildGateway: %v", err)
		}
		t.Cleanup(func() { _ = gw.Close() })

		entry := findEntry(t, buf, "plugins loaded")
		if got := entry["count"]; got != float64(1) {
			t.Errorf("plugins loaded count = %v, want 1 — the two disabled entries load nothing", got)
		}
	})

	t.Run("startup banner", func(t *testing.T) {
		cfg := pluginCountConfig()
		banner := captureStderr(t, func() {
			PrintStartupBanner(":8080", providers.NewRegistry(), &cfg, "", BackendSQLite, BackendSQLite)
		})
		if !strings.Contains(banner, "| 1 plugins") {
			t.Errorf("banner does not report the one enabled plugin:\n%s", banner)
		}
	})
}

// captureStderr returns what fn writes to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	read := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		read <- buf.String()
	}()
	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	return <-read
}
