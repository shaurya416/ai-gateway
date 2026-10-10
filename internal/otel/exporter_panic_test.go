package otel

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/observability"
	"github.com/ferro-labs/ai-gateway/pkg/logger"
)

// lifecyclePanicExporter is a third-party exporter that breaks outside Export:
// its Init or its Shutdown panics, the way one does that type-asserts a config
// key without checking it was set.
type lifecyclePanicExporter struct {
	name          string
	panicInit     bool
	panicShutdown bool
}

func (e *lifecyclePanicExporter) Name() string { return e.name }

func (e *lifecyclePanicExporter) Init(_ context.Context, cfg map[string]any) error {
	if e.panicInit {
		_ = cfg["api_key"].(string)
	}
	return nil
}

func (e *lifecyclePanicExporter) Export(_ context.Context, _ observability.Event) error {
	return nil
}

func (e *lifecyclePanicExporter) Shutdown(_ context.Context) error {
	if e.panicShutdown {
		panic("exporter shutdown blew up")
	}
	return nil
}

// captureLogs routes the default logger into a buffer for the test's lifetime.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	var logs lockedBuffer
	original := logger.Default()
	logger.SetDefault(logger.New(logger.Options{Level: "info", Output: &logs}))
	t.Cleanup(func() { logger.SetDefault(original) })
	return &logs
}

// A misconfigured exporter is skipped, not fatal — including one whose factory
// or Init panics rather than returning an error. The exporters behind it are
// still attached and still receive events.
func TestInitExporterPanicIsSkipped(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	cases := []struct {
		name     string
		register func(name string)
	}{
		{
			name: "Init panics",
			register: func(name string) {
				observability.RegisterExporter(name, func() observability.Exporter {
					return &lifecyclePanicExporter{name: name, panicInit: true}
				})
			},
		},
		{
			name: "factory panics",
			register: func(name string) {
				observability.RegisterExporter(name, func() observability.Exporter {
					panic("factory blew up")
				})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)

			brokenName := uniqueExporterName(t, "broken")
			tc.register(brokenName)
			healthyName := uniqueExporterName(t, "healthy")
			healthy := &fakeExporter{}
			observability.RegisterExporter(healthyName, func() observability.Exporter { return healthy })

			cfg := DefaultConfig()
			cfg.Enabled = true
			cfg.Endpoint = ""
			cfg.Exporters = []ExporterConfig{
				{Name: brokenName, Enabled: true},
				{Name: healthyName, Enabled: true},
			}

			var (
				prov     observability.Provider
				shutdown ShutdownFunc
				err      error
			)
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						t.Fatalf("Init panicked instead of skipping the broken exporter: %v", recovered)
					}
				}()
				prov, shutdown, err = Init(context.Background(), cfg)
			}()
			if err != nil {
				t.Fatalf("Init returned error: %v", err)
			}

			prov.RecordEvent(context.Background(), observability.Event{Subject: "gateway.request.completed"})
			shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := shutdown(shutCtx); err != nil {
				t.Fatalf("shutdown returned error: %v", err)
			}

			healthy.mu.Lock()
			delivered := len(healthy.exported)
			healthy.mu.Unlock()
			if delivered != 1 {
				t.Errorf("exporter behind the broken one received %d events, want 1", delivered)
			}

			logged := logs.String()
			if !strings.Contains(logged, "panicked") || !strings.Contains(logged, brokenName) {
				t.Errorf("the panic was not logged against exporter %q: %q", brokenName, logged)
			}
		})
	}
}

// An exporter whose Shutdown panics must not take the process down on its way
// out, nor keep the exporters behind it — or the TracerProvider drain that
// follows — from shutting down. The failure is reported as an error.
func TestShutdownExporterPanicIsContained(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	logs := captureLogs(t)

	brokenName := uniqueExporterName(t, "broken-shutdown")
	observability.RegisterExporter(brokenName, func() observability.Exporter {
		return &lifecyclePanicExporter{name: brokenName, panicShutdown: true}
	})
	healthyName := uniqueExporterName(t, "healthy")
	healthy := &fakeExporter{}
	observability.RegisterExporter(healthyName, func() observability.Exporter { return healthy })

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Endpoint = ""
	cfg.Exporters = []ExporterConfig{
		{Name: brokenName, Enabled: true},
		{Name: healthyName, Enabled: true},
	}

	_, shutdown, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var shutdownErr error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("shutdown panicked: %v", recovered)
			}
		}()
		shutdownErr = shutdown(shutCtx)
	}()

	if shutdownErr == nil || !strings.Contains(shutdownErr.Error(), brokenName) {
		t.Errorf("shutdown error = %v, want one naming exporter %q", shutdownErr, brokenName)
	}
	healthy.mu.Lock()
	shutCalled := healthy.shutdownCalled
	healthy.mu.Unlock()
	if !shutCalled {
		t.Error("the exporter behind the panicking one was never shut down")
	}
	if logged := logs.String(); !strings.Contains(logged, "panicked") || !strings.Contains(logged, brokenName) {
		t.Errorf("the panic was not logged against exporter %q: %q", brokenName, logged)
	}
}
