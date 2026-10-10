// Package otel wires the gateway core to OpenTelemetry.
//
// This is the ONLY internal package permitted to import the
// OpenTelemetry SDK. Everywhere else in the gateway depends on the
// public observability package, which exposes the OTel-independent
// Provider, Span, and Exporter interfaces.
//
// Init returns observability.NoOp only when tracing is disabled or when
// neither an OTLP endpoint nor an enabled exporter is configured; otherwise
// it returns the OTel-backed provider, with an OTLP span pipeline when an
// endpoint is set.
//
// Callers MUST always invoke the ShutdownFunc returned by Init from
// the gateway's graceful-shutdown sequence.
package otel
