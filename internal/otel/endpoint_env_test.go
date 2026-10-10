package otel

import (
	"strings"
	"testing"
)

// TestBareHostPortEnvEndpointExports is the regression test for the form the
// quick start and the full-stack compose file both use —
// OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317, jaeger:4317 — exporting nothing.
//
// The variable was handed to the SDK unread, and the SDK parses it as a URL: a
// bare IP:port fails to parse and is dropped for the default collector address,
// and a bare name:port parses with the host name as its *scheme* and no host at
// all, so the exporter dialled nowhere, over TLS. The gateway logged that it was
// exporting to the endpoint the environment named.
func TestBareHostPortEnvEndpointExports(t *testing.T) {
	c := newCollectorStub(t)
	t.Setenv(envEndpoint, strings.TrimPrefix(c.URL, "http://"))

	exportOneSpan(t, Config{Enabled: true, Protocol: "http/protobuf"})

	if got, want := c.awaitPath(t), "/v1/traces"; got != want {
		t.Errorf("exported to %q, want %q — a bare host:port is a base endpoint", got, want)
	}
}

// TestBareHostPortEnvEndpointResolves pins the resolution for both variables
// and both transports, including a host *name*, which is the form the SDK
// misreads as a scheme.
func TestBareHostPortEnvEndpointResolves(t *testing.T) {
	tests := []struct {
		name   string
		env    map[string]string
		http   bool
		want   exportTarget
		reason string
	}{
		{
			name: "base variable over gRPC",
			env:  map[string]string{envEndpoint: "jaeger:4317"},
			want: exportTarget{hostPort: "jaeger:4317"},
		},
		{
			name: "signal variable over gRPC",
			env:  map[string]string{envTracesEndpoint: "jaeger:4317"},
			want: exportTarget{hostPort: "jaeger:4317"},
		},
		{
			name: "base variable over HTTP gets the signal path from the SDK",
			env:  map[string]string{envEndpoint: "collector:4318"},
			http: true,
			want: exportTarget{hostPort: "collector:4318"},
		},
		{
			name:   "signal variable over HTTP is used as written, at the root path",
			env:    map[string]string{envTracesEndpoint: "collector:4318"},
			http:   true,
			want:   exportTarget{url: "http://collector:4318/"},
			reason: "the specification gives a signal endpoint with no path the root path",
		},
		{
			name: "signal variable outranks the base one",
			env:  map[string]string{envEndpoint: "base:4317", envTracesEndpoint: "signal:4317"},
			want: exportTarget{hostPort: "signal:4317"},
		},
		{
			name: "a URL is still left to the SDK",
			env:  map[string]string{envEndpoint: "http://collector:4317"},
			want: exportTarget{},
		},
		{
			name:   "a bare host with no port is still left to the SDK",
			env:    map[string]string{envEndpoint: "collector.example.com"},
			want:   exportTarget{},
			reason: "the SDK already reads it, dialling gRPC over TLS on the default port",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string { return tc.env[k] }
			got, err := resolveExportTarget(Config{Endpoint: "http://never-used:1"}, getenv, tc.http)
			if err != nil {
				t.Fatalf("resolveExportTarget: %v", err)
			}
			if got != tc.want {
				t.Fatalf("target = %+v, want %+v %s", got, tc.want, tc.reason)
			}
		})
	}
}

// TestBareEnvEndpointWithPathIsRejected keeps the rule the configured endpoint
// already follows: a scheme-less value carrying a path cannot be understood, and
// is refused at startup rather than exporting nowhere.
func TestBareEnvEndpointWithPathIsRejected(t *testing.T) {
	getenv := func(k string) string {
		if k == envEndpoint {
			return "collector:4318/v1/traces"
		}
		return ""
	}
	if _, err := resolveExportTarget(Config{}, getenv, true); err == nil {
		t.Fatal("resolveExportTarget accepted a scheme-less endpoint with a path")
	}
}
