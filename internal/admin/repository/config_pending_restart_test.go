package repository

import (
	"context"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
)

// tracingTo returns baseConfig with tracing exporting to endpoint.
func tracingTo(endpoint string) config.Config {
	enabled := true
	cfg := baseConfig()
	cfg.Observability.Tracing.Enabled = &enabled
	cfg.Observability.Tracing.Endpoint = endpoint
	return cfg
}

// The tracing pipeline is built once, from the observability section of the
// config the process starts on, so a pending restart is a difference from THAT
// section — not from whatever the previous apply happened to hold. Comparing
// against the previous apply got two cases backwards.
//
// Putting the started section back — undoing a change that was awaiting a
// restart — warned that it was not applied and would take effect on the next
// restart, when it is exactly what the process is exporting with.
//
// A reset — DELETE /admin/config — never compared at all. With a stored
// override adopted at start, the pipeline runs the override's section; the
// reset makes GET /admin/config serve the file's while the process goes on
// exporting with the override's, and nothing said so.
func TestPendingRestartIsMeasuredAgainstTheStartedConfig(t *testing.T) {
	t.Run("restoring the started section reports nothing pending", func(t *testing.T) {
		gw, err := newTestGateway(t, baseConfig())
		if err != nil {
			t.Fatalf("new gateway: %v", err)
		}
		mgr, err := NewGatewayConfigManager(gw, &successConfigStore{})
		if err != nil {
			t.Fatalf("new config manager: %v", err)
		}
		if err := mgr.ReloadConfig(context.Background(), tracingTo("collector:4317")); err != nil {
			t.Fatalf("ReloadConfig: %v", err)
		}

		buf := captureLogger(t)
		if err := mgr.ReloadConfig(context.Background(), baseConfig()); err != nil {
			t.Fatalf("ReloadConfig back to the started config: %v", err)
		}
		if strings.Contains(buf.String(), "pending_restart") {
			t.Errorf("restoring the section the process started with reported a pending restart:\n%s", buf.String())
		}
	})

	t.Run("a reset away from an adopted override reports the restart", func(t *testing.T) {
		gw, err := newTestGateway(t, baseConfig())
		if err != nil {
			t.Fatalf("new gateway: %v", err)
		}
		mgr, err := NewGatewayConfigManager(gw, &successConfigStore{cfg: tracingTo("collector:4317")})
		if err != nil {
			t.Fatalf("new config manager: %v", err)
		}
		if !mgr.AdoptedStoredConfig() {
			t.Fatal("this case needs the stored override adopted at start")
		}

		buf := captureLogger(t)
		if err := mgr.ResetConfig(context.Background()); err != nil {
			t.Fatalf("ResetConfig: %v", err)
		}
		entry := findLog(t, buf, "observability config accepted and persisted but not applied")
		if entry["pending_restart"] != true {
			t.Errorf("pending_restart = %v, want true", entry["pending_restart"])
		}
	})

	t.Run("a reset back to the started section reports nothing pending", func(t *testing.T) {
		gw, err := newTestGateway(t, baseConfig())
		if err != nil {
			t.Fatalf("new gateway: %v", err)
		}
		mgr, err := NewGatewayConfigManager(gw, &successConfigStore{})
		if err != nil {
			t.Fatalf("new config manager: %v", err)
		}
		if err := mgr.ReloadConfig(context.Background(), tracingTo("collector:4317")); err != nil {
			t.Fatalf("ReloadConfig: %v", err)
		}

		buf := captureLogger(t)
		if err := mgr.ResetConfig(context.Background()); err != nil {
			t.Fatalf("ResetConfig: %v", err)
		}
		if strings.Contains(buf.String(), "pending_restart") {
			t.Errorf("a reset to the section the process started with reported a pending restart:\n%s", buf.String())
		}
	})

	t.Run("a further change while one is pending still reports it", func(t *testing.T) {
		gw, err := newTestGateway(t, baseConfig())
		if err != nil {
			t.Fatalf("new gateway: %v", err)
		}
		mgr, err := NewGatewayConfigManager(gw, &successConfigStore{})
		if err != nil {
			t.Fatalf("new config manager: %v", err)
		}
		if err := mgr.ReloadConfig(context.Background(), tracingTo("collector:4317")); err != nil {
			t.Fatalf("ReloadConfig: %v", err)
		}

		buf := captureLogger(t)
		if err := mgr.ReloadConfig(context.Background(), tracingTo("other-collector:4317")); err != nil {
			t.Fatalf("ReloadConfig: %v", err)
		}
		findLog(t, buf, "observability config accepted and persisted but not applied")
	})
}
