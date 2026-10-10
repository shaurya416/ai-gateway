package tracingpolicy

import (
	"fmt"
	"net/url"
	"strings"
)

// OTLP transports the exporter supports. ProtocolHTTP is accepted as a
// shorthand for ProtocolHTTPProtobuf. An empty string is accepted and treated
// as the default (gRPC) by the tracing backend.
const (
	ProtocolGRPC         = "grpc"
	ProtocolHTTPProtobuf = "http/protobuf"
	ProtocolHTTP         = "http"
)

// ValidateProtocol reports an error when protocol is not an OTLP transport the
// exporter supports. Anything else used to select gRPC silently, so a value
// naming an HTTP collector — "http/json", which the exporter does not
// implement, or "HTTP/protobuf" — sent every export over the wrong transport.
func ValidateProtocol(protocol string) error {
	switch protocol {
	case "", ProtocolGRPC, ProtocolHTTPProtobuf, ProtocolHTTP:
		return nil
	default:
		return fmt.Errorf("invalid protocol %q: must be one of grpc, http/protobuf", protocol)
	}
}

// ValidateSampleRatio reports an error when ratio is not a fraction between 0.0
// and 1.0 inclusive. An out-of-range ratio used to be clamped, which reads one
// way and samples another: 10, meant as ten percent, sampled every trace, and a
// negative ratio sampled none while tracing reported itself enabled. NaN is
// rejected with them.
func ValidateSampleRatio(ratio float64) error {
	if ratio >= 0 && ratio <= 1 {
		return nil
	}
	return fmt.Errorf("invalid sample_ratio %v: must be between 0.0 and 1.0", ratio)
}

// ValidateEndpoint reports an error when endpoint — observability.tracing.endpoint
// — is a value the exporter cannot use: a scheme-less value carrying a path, a
// URL that does not parse, a scheme other than http or https, or a URL with no
// host. An empty endpoint is accepted: tracing is then driven by the
// OTEL_EXPORTER_OTLP_* environment, or not at all. A bare host:port is accepted
// too; OTLP/gRPC endpoints are conventionally written that way.
//
// The tracing backend refuses the same values when it starts, so checking them
// here as well is what makes `ferrogw validate` give the answer startup gives,
// rather than passing a config the gateway then refuses to serve.
func ValidateEndpoint(endpoint string) error {
	raw := strings.TrimSpace(endpoint)
	if raw == "" {
		return nil
	}
	if !strings.Contains(raw, "://") {
		if strings.ContainsAny(raw, "/?#") {
			return fmt.Errorf(
				"tracing endpoint %q has a path but no scheme: write it as a URL (http://host:port/...) or as a bare host:port", raw)
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("tracing endpoint %q is not a valid URL: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("tracing endpoint %q has scheme %q: only http and https are supported", raw, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("tracing endpoint %q has no host", raw)
	}
	return nil
}
