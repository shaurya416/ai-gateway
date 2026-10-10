package otel

import (
	"context"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/internal/version"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// TestExportedResourceCarriesBuildVersion is the regression test for every
// exported span naming no release. The resource stamped service.version with
// the empty string whatever version the binary was linked as, so a backend
// grouping traces by service.version put every deployment under "".
func TestExportedResourceCarriesBuildVersion(t *testing.T) {
	const linked = "v9.8.7-resource-test"
	orig := version.Version
	version.Version = linked
	t.Cleanup(func() { version.Version = orig })

	// Only the configured endpoint and the binary's version may decide this.
	for _, name := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES",
	} {
		t.Setenv(name, "")
	}

	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	collector := newCollectorStub(t)
	_, shutdown, err := Init(context.Background(), Config{
		Enabled:     true,
		Protocol:    "http/protobuf",
		Endpoint:    collector.URL,
		SampleRatio: 1,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	})

	tp, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider)
	if !ok {
		t.Fatalf("global tracer provider is %T, want the SDK provider Init installs", otel.GetTracerProvider())
	}
	recorder := tracetest.NewSpanRecorder()
	tp.RegisterSpanProcessor(recorder)

	_, span := tp.Tracer("resource_test").Start(context.Background(), "probe")
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	got, ok := ended[0].Resource().Set().Value(semconv.ServiceVersionKey)
	if !ok || got.AsString() != linked {
		t.Fatalf("resource service.version = %q (present %v), want the linked version %q", got.AsString(), ok, linked)
	}
}
