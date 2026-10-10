package otel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/observability"
	"go.opentelemetry.io/otel"
)

// hungShutdownExporter is a third-party exporter whose Shutdown ignores the
// context it is handed and blocks until release is closed — one still flushing
// to a backend that stopped answering, over a client with no timeout of its own.
type hungShutdownExporter struct {
	name    string
	release <-chan struct{}
}

func (e *hungShutdownExporter) Name() string { return e.name }

func (e *hungShutdownExporter) Init(context.Context, map[string]any) error { return nil }

func (e *hungShutdownExporter) Export(context.Context, observability.Event) error { return nil }

func (e *hungShutdownExporter) Shutdown(context.Context) error {
	<-e.release
	return nil
}

// TestShutdownIsBoundedByAnExporterThatIgnoresItsDeadline is the regression
// test for an exporter's Shutdown holding the whole shutdown hostage.
//
// Provider.Shutdown promises never to block past its deadline, and the drain in
// front of it already stops waiting on an Export that ignores the context. The
// exporter's own Shutdown was awaited unconditionally, so one that ignored the
// context never returned: the TracerProvider flush queued behind it never ran,
// and every span still buffered was lost when the orchestrator killed the
// process.
func TestShutdownIsBoundedByAnExporterThatIgnoresItsDeadline(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	collector := newCollectorStub(t)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	hungName := uniqueExporterName(t, "hung-shutdown")
	observability.RegisterExporter(hungName, func() observability.Exporter {
		return &hungShutdownExporter{name: hungName, release: release}
	})
	healthyName := uniqueExporterName(t, "healthy")
	healthy := &fakeExporter{}
	observability.RegisterExporter(healthyName, func() observability.Exporter { return healthy })

	_, shutdown, err := Init(context.Background(), Config{
		Enabled:       true,
		Protocol:      "http/protobuf",
		Endpoint:      collector.URL,
		SampleRatio:   1,
		ShutdownGrace: 200 * time.Millisecond,
		Exporters: []ExporterConfig{
			{Name: hungName, Enabled: true},
			{Name: healthyName, Enabled: true},
		},
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	// A span still buffered in the batch processor when shutdown begins.
	_, span := otel.GetTracerProvider().Tracer("exporter_shutdown_test").Start(context.Background(), "buffered")
	span.End()

	done := make(chan error, 1)
	go func() { done <- shutdown(context.Background()) }()
	var shutdownErr error
	select {
	case shutdownErr = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not return: an exporter whose Shutdown ignores its deadline held it past shutdown_grace")
	}

	if !errors.Is(shutdownErr, context.DeadlineExceeded) || !strings.Contains(shutdownErr.Error(), hungName) {
		t.Errorf("shutdown error = %v, want one naming exporter %q and wrapping the deadline", shutdownErr, hungName)
	}

	// The TracerProvider stage still ran: the buffered span was flushed.
	collector.awaitPath(t)

	// The exporter behind the hung one was still asked to shut down.
	deadline := time.Now().Add(2 * time.Second)
	for {
		healthy.mu.Lock()
		called := healthy.shutdownCalled
		healthy.mu.Unlock()
		if called {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the exporter behind the hung one was never shut down")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
