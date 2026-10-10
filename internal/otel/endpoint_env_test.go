package otel

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TestBareHostPortEnvEndpointExports is the regression test for the form the
// quick start and the full-stack compose file both use —
// OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317, jaeger:4317 — exporting nothing.
//
// The variable was handed to the SDK unread, and the SDK parses it as a URL: a
// bare IP:port fails to parse and is dropped for the default collector address,
// and a bare name:port parses with the host name as its *scheme* and no host at
// all, so the exporter dialled nowhere, over TLS. The gateway logged that it was
// exporting to the endpoint the environment named. The stub collector is
// plaintext, which a scheme-less endpoint reaches only when
// OTEL_EXPORTER_OTLP_INSECURE says so.
func TestBareHostPortEnvEndpointExports(t *testing.T) {
	c := newCollectorStub(t)
	t.Setenv(envEndpoint, strings.TrimPrefix(c.URL, "http://"))
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")

	exportOneSpan(t, Config{Enabled: true, Protocol: "http/protobuf"})

	if got, want := c.awaitPath(t), "/v1/traces"; got != want {
		t.Errorf("exported to %q, want %q — a bare host:port is a base endpoint", got, want)
	}
}

// TestBareHostPortEnvEndpointResolves pins the resolution for both variables
// and both transports, including a host *name*, which is the form the SDK
// misreads as a scheme. Each case opts into plaintext; the transport rule
// itself is TestBareHostPortEnvEndpointHonoursInsecure's.
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
			env:  map[string]string{envEndpoint: "jaeger:4317", "OTEL_EXPORTER_OTLP_INSECURE": "true"},
			want: exportTarget{url: "http://jaeger:4317"},
		},
		{
			name: "signal variable over gRPC",
			env:  map[string]string{envTracesEndpoint: "jaeger:4317", "OTEL_EXPORTER_OTLP_INSECURE": "true"},
			want: exportTarget{url: "http://jaeger:4317"},
		},
		{
			name: "base variable over HTTP gets the signal path appended",
			env:  map[string]string{envEndpoint: "collector:4318", "OTEL_EXPORTER_OTLP_INSECURE": "true"},
			http: true,
			want: exportTarget{url: "http://collector:4318/v1/traces"},
		},
		{
			name:   "signal variable over HTTP is used as written, at the root path",
			env:    map[string]string{envTracesEndpoint: "collector:4318", "OTEL_EXPORTER_OTLP_INSECURE": "true"},
			http:   true,
			want:   exportTarget{url: "http://collector:4318/"},
			reason: "the specification gives a signal endpoint with no path the root path",
		},
		{
			name: "signal variable outranks the base one",
			env:  map[string]string{envEndpoint: "base:4317", envTracesEndpoint: "signal:4317", "OTEL_EXPORTER_OTLP_INSECURE": "true"},
			want: exportTarget{url: "http://signal:4317"},
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

// TestBareHostPortEnvEndpointHonoursInsecure holds a bare host:port in the
// environment to the specification's transport rule: OTEL_EXPORTER_OTLP_INSECURE
// decides, the signal-specific variable outranks it, and it defaults to false —
// TLS. The gateway forced plaintext instead, overriding an operator's explicit
// INSECURE=false and sending OTEL_EXPORTER_OTLP_HEADERS in the clear.
func TestBareHostPortEnvEndpointHonoursInsecure(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		http bool
		want exportTarget
	}{
		{
			name: "unset is TLS over gRPC",
			env:  map[string]string{envEndpoint: "otlp.vendor.example.com:4317"},
			want: exportTarget{url: "https://otlp.vendor.example.com:4317"},
		},
		{
			name: "false is TLS over gRPC",
			env:  map[string]string{envEndpoint: "otlp.vendor.example.com:4317", "OTEL_EXPORTER_OTLP_INSECURE": "false"},
			want: exportTarget{url: "https://otlp.vendor.example.com:4317"},
		},
		{
			name: "true is plaintext over gRPC",
			env:  map[string]string{envEndpoint: "jaeger:4317", "OTEL_EXPORTER_OTLP_INSECURE": "TRUE"},
			want: exportTarget{url: "http://jaeger:4317"},
		},
		{
			name: "the signal-specific variable outranks the base one",
			env:  map[string]string{envEndpoint: "jaeger:4317", "OTEL_EXPORTER_OTLP_INSECURE": "true", "OTEL_EXPORTER_OTLP_TRACES_INSECURE": "false"},
			want: exportTarget{url: "https://jaeger:4317"},
		},
		{
			name: "unset is TLS over HTTP, signal path appended to the base",
			env:  map[string]string{envEndpoint: "collector:4318"},
			http: true,
			want: exportTarget{url: "https://collector:4318/v1/traces"},
		},
		{
			name: "true is plaintext over HTTP, signal endpoint at the root path",
			env:  map[string]string{envTracesEndpoint: "collector:4318", "OTEL_EXPORTER_OTLP_TRACES_INSECURE": "true"},
			http: true,
			want: exportTarget{url: "http://collector:4318/"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string { return tc.env[k] }
			got, err := resolveExportTarget(Config{}, getenv, tc.http)
			if err != nil {
				t.Fatalf("resolveExportTarget: %v", err)
			}
			if got != tc.want {
				t.Fatalf("target = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestBareHostPortEnvEndpointOpensTLSOnTheWire asserts the transport from the
// outside: with OTEL_EXPORTER_OTLP_INSECURE unset or false the first bytes the
// gRPC exporter writes are a TLS handshake record, never the plaintext HTTP/2
// preface that would precede the exporter headers.
func TestBareHostPortEnvEndpointOpensTLSOnTheWire(t *testing.T) {
	for _, insecure := range []string{"", "false"} {
		t.Run("insecure="+insecure, func(t *testing.T) {
			lis, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			t.Cleanup(func() { _ = lis.Close() })
			first := make(chan []byte, 1)
			go func() {
				conn, err := lis.Accept()
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				b := make([]byte, 5)
				n, _ := io.ReadFull(conn, b)
				first <- b[:n]
			}()

			t.Setenv(envEndpoint, lis.Addr().String())
			t.Setenv(envTracesEndpoint, "")
			t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", insecure)
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_INSECURE", "")
			t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer probe-token")

			exporter, err := newSpanExporter(context.Background(), Config{Enabled: true, Protocol: "grpc"})
			if err != nil {
				t.Fatalf("newSpanExporter: %v", err)
			}
			tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter))
			_, span := tp.Tracer("endpoint_env_test").Start(context.Background(), "probe")
			span.End()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = tp.ForceFlush(ctx) // the handshake fails against this listener; only the bytes matter
			_ = tp.Shutdown(ctx)

			select {
			case b := <-first:
				if len(b) == 0 || b[0] != 0x16 {
					t.Fatalf("first bytes on the wire = %q, want a TLS handshake record (0x16)", b)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the exporter never connected")
			}
		})
	}
}
