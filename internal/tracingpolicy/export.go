package tracingpolicy

import "fmt"

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
