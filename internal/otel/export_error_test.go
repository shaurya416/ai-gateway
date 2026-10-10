package otel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/observability"
)

// rejectingExporter is an exporter whose backend refuses every event — a
// revoked API key, an endpoint that has moved. Export returns the error rather
// than panicking, which is what a well-behaved exporter does.
type rejectingExporter struct{ name string }

func (e *rejectingExporter) Name() string                                   { return e.name }
func (e *rejectingExporter) Init(_ context.Context, _ map[string]any) error { return nil }
func (e *rejectingExporter) Export(_ context.Context, _ observability.Event) error {
	return errors.New("backend answered 401: key revoked, contact ops@example.com")
}
func (e *rejectingExporter) Shutdown(_ context.Context) error { return nil }

// An exporter whose every Export fails must say so. The error used to be
// discarded, so the integration stayed attached, reported nothing, and lost
// every event — indistinguishable from one that was working with no traffic.
// The report is sampled like the queue-full warning (the first failure, then
// every 64th), names the exporter, carries the error redacted, and is not
// raised for an exporter that succeeds.
func TestRecordEvent_ExportErrorIsReported(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	logs := captureLogs(t)

	failingName := uniqueExporterName(t, "rejecting")
	observability.RegisterExporter(failingName, func() observability.Exporter {
		return &rejectingExporter{name: failingName}
	})
	healthyName := uniqueExporterName(t, "healthy")
	healthy := &fakeExporter{}
	observability.RegisterExporter(healthyName, func() observability.Exporter { return healthy })

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Endpoint = ""
	cfg.Exporters = []ExporterConfig{
		{Name: failingName, Enabled: true},
		{Name: healthyName, Enabled: true},
	}
	prov, shutdown, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}

	// 64 failures: reported at the 1st and the 64th, and nowhere in between.
	const sent = 64
	for i := 0; i < sent; i++ {
		prov.RecordEvent(context.Background(), observability.Event{Subject: "gateway.request.completed"})
	}

	// Shutdown drains the queue and waits for the worker, so every export and
	// every report has happened by the time it returns.
	shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := shutdown(shutCtx); err != nil {
		t.Fatalf("shutdown returned error: %v", err)
	}

	healthy.mu.Lock()
	delivered := len(healthy.exported)
	healthy.mu.Unlock()
	if delivered != sent {
		t.Fatalf("exporter behind the failing one received %d events, want %d", delivered, sent)
	}

	var reports []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "failed to export event") {
			reports = append(reports, line)
		}
	}
	if len(reports) != 2 {
		t.Fatalf("got %d export-failure reports for %d failures, want 2 (the 1st and the 64th):\n%s",
			len(reports), sent, logs.String())
	}
	for i, wantTotal := range []string{`"total_failed":1`, `"total_failed":64`} {
		line := reports[i]
		if !strings.Contains(line, `"level":"ERROR"`) {
			t.Errorf("report %d is not at error level: %s", i, line)
		}
		if !strings.Contains(line, failingName) {
			t.Errorf("report %d does not name the exporter %q: %s", i, failingName, line)
		}
		if !strings.Contains(line, "key revoked") {
			t.Errorf("report %d does not carry the exporter's error: %s", i, line)
		}
		if strings.Contains(line, "ops@example.com") {
			t.Errorf("report %d carries the exporter's error unredacted: %s", i, line)
		}
		if !strings.Contains(line, wantTotal) {
			t.Errorf("report %d does not carry %s: %s", i, wantTotal, line)
		}
		if strings.Contains(line, healthyName) {
			t.Errorf("report %d names the exporter that succeeded: %s", i, line)
		}
	}
}
