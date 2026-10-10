package otel

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"

	"github.com/ferro-labs/ai-gateway/internal/tracingpolicy"
	"github.com/ferro-labs/ai-gateway/pkg/logger"
)

// The standard OTLP endpoint variables. A URL in either is handed to the SDK
// uninterpreted; only a bare host:port, which the SDK cannot read, is resolved
// by the gateway — see resolveExportTarget.
const (
	envEndpoint       = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envTracesEndpoint = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
)

// The standard variables that decide transport security for an endpoint
// written without a scheme. The signal-specific one outranks the other.
const (
	envInsecure       = "OTEL_EXPORTER_OTLP_INSECURE"
	envTracesInsecure = "OTEL_EXPORTER_OTLP_TRACES_INSECURE"
)

// tracesSignalPath is the path the OTLP/HTTP traces signal is sent to, relative
// to a base endpoint. Fixed by the specification, not configurable.
const tracesSignalPath = "v1/traces"

// exportTarget is how the span exporter is pointed at a collector.
//
// The zero value means "pass no endpoint option at all", which hands a URL in
// OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT to the
// SDK's own handling of it. That is deliberate: those two variables have
// precise and *different* semantics in the specification — the first is a base
// URL that v1/traces is appended to, the second is used verbatim — and the SDK
// already implements both correctly. Re-deriving them here is how the gateway
// previously turned a collector URL into a percent-encoded hostname and
// exported nothing.
type exportTarget struct {
	// url is a complete endpoint URL, scheme and path included, ready to hand
	// to WithEndpointURL.
	url string
	// hostPort is a bare host:port with no scheme from
	// observability.tracing.endpoint, which has always meant plaintext there
	// and still does.
	hostPort string
}

// log reports the resolved endpoint spans are being sent to. It is the URL the
// exporter will actually POST to, signal path included, so an operator can
// compare it against their collector without deriving it.
func (t exportTarget) log() {
	switch {
	case t.url != "":
		logger.Default().Info("otel: exporting traces", "endpoint", t.url)
	case t.hostPort != "":
		logger.Default().Info("otel: exporting traces", "endpoint", t.hostPort, "transport_security", "plaintext")
	default:
		logger.Default().Info("otel: exporting traces to the endpoint named by the OTEL_EXPORTER_OTLP_* environment")
	}
}

// isHTTPProtocol reports whether the configured protocol selects OTLP/HTTP.
func isHTTPProtocol(protocol string) bool {
	return protocol == tracingpolicy.ProtocolHTTPProtobuf || protocol == tracingpolicy.ProtocolHTTP
}

// resolveExportTarget decides how to point the exporter at a collector,
// following the OTLP exporter specification
// (https://opentelemetry.io/docs/specs/otel/protocol/exporter/).
//
// Precedence is unchanged: the standard environment variables win over the
// configured endpoint, and the signal-specific one wins over the base one. A
// variable holding a URL is not parsed here: the zero exportTarget passes no
// endpoint option, and the SDK applies the specification's rules itself —
// appending v1/traces to the non-signal-specific OTEL_EXPORTER_OTLP_ENDPOINT,
// using OTEL_EXPORTER_OTLP_TRACES_ENDPOINT verbatim, and honouring the scheme.
// A variable holding a bare host:port is the exception; see envExportTarget.
//
// observability.tracing.endpoint is the configured analogue of
// OTEL_EXPORTER_OTLP_ENDPOINT and is therefore treated the same way: a base
// endpoint, with the traces signal path appended for OTLP/HTTP. Giving it a
// third, gateway-specific meaning would leave operators unable to reason about
// their own collector's documentation. It accepts one thing the environment
// variable does not — a base that already ends in the signal path is used as
// written rather than having it appended twice; see parseConfiguredEndpoint.
func resolveExportTarget(cfg Config, getenv func(string) string, httpProtocol bool) (exportTarget, error) {
	if v := strings.TrimSpace(getenv(envTracesEndpoint)); v != "" {
		return envExportTarget(envTracesEndpoint, v, httpProtocol, true, envInsecureSet(getenv))
	}
	if v := strings.TrimSpace(getenv(envEndpoint)); v != "" {
		return envExportTarget(envEndpoint, v, httpProtocol, false, envInsecureSet(getenv))
	}
	return parseConfiguredEndpoint(cfg.Endpoint, httpProtocol)
}

// envInsecureSet reports whether the environment opts a scheme-less endpoint
// out of transport security, read as the SDK reads it: the signal-specific
// variable outranks the base one, and only "true", in any case, is true.
func envInsecureSet(getenv func(string) string) bool {
	v := strings.TrimSpace(getenv(envTracesInsecure))
	if v == "" {
		v = strings.TrimSpace(getenv(envInsecure))
	}
	return strings.EqualFold(v, "true")
}

// envExportTarget resolves the value of one standard endpoint variable.
//
// A URL goes to the SDK unread. A bare host:port does not, because the SDK
// cannot read it: it parses the value as a URL, so "127.0.0.1:4317" fails to
// parse and is silently replaced by the default collector address, and
// "jaeger:4317" parses with the host name as its scheme and no host at all —
// the exporter then dials nowhere, over TLS, while the gateway reports that it
// is exporting. So it is resolved here, to a URL whose scheme carries the
// transport the specification assigns a scheme-less endpoint:
// OTEL_EXPORTER_OTLP_INSECURE (or its signal-specific form) set to true is
// plaintext, and anything else — unset included — is TLS. Plaintext is never
// assumed: the SDK still sends OTEL_EXPORTER_OTLP_HEADERS, which commonly
// carries a collector credential. The signal-specific variable keeps its own
// path rule — a signal endpoint with no path is sent to the root path — so under
// OTLP/HTTP it becomes that URL rather than a base.
//
// A value with no port is left to the SDK as well: it reads a bare host as a
// path, which gRPC dials over TLS on its default port, and that already works.
//
// A scheme-less host:port carrying a path is refused, as it is for the
// configured endpoint: there is no reading of it the exporter can use.
func envExportTarget(name, v string, httpProtocol, signal, insecure bool) (exportTarget, error) {
	if strings.Contains(v, "://") {
		return exportTarget{}, nil
	}
	if _, _, err := net.SplitHostPort(v); err != nil {
		return exportTarget{}, nil
	}
	if strings.ContainsAny(v, "/?#") {
		return exportTarget{}, fmt.Errorf(
			"%s %q has a path but no scheme: write it as a URL (http://host:port/...) or as a bare host:port", name, v)
	}
	scheme := "https"
	if insecure {
		scheme = "http"
	}
	switch {
	case httpProtocol && signal:
		return exportTarget{url: scheme + "://" + v + "/"}, nil
	case httpProtocol:
		return exportTarget{url: scheme + "://" + v + "/" + tracesSignalPath}, nil
	default:
		return exportTarget{url: scheme + "://" + v}, nil
	}
}

// parseConfiguredEndpoint validates observability.tracing.endpoint and resolves
// it to an exportTarget. An empty endpoint is not an error — it means tracing is
// driven by the environment, or not at all.
//
// A value that cannot be understood is rejected rather than passed on. An
// endpoint the exporter cannot use is not a degraded configuration, it is a
// silently disabled one: the export fails per batch, deep inside the SDK, long
// after the operator has stopped watching the logs.
func parseConfiguredEndpoint(raw string, httpProtocol bool) (exportTarget, error) {
	// The same check config validation runs, so a value refused here was
	// already refused by `ferrogw validate`.
	if err := tracingpolicy.ValidateEndpoint(raw); err != nil {
		return exportTarget{}, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return exportTarget{}, nil
	}

	// No scheme: a bare host:port, which has always meant plaintext here.
	// OTLP/gRPC endpoints are conventionally written this way and the
	// specification allows any form the gRPC client accepts, so this stays.
	if !strings.Contains(raw, "://") {
		return exportTarget{hostPort: raw}, nil
	}

	u, err := url.Parse(raw)
	if err != nil {
		return exportTarget{}, fmt.Errorf("tracing endpoint %q is not a valid URL: %w", raw, err)
	}

	// OTLP/gRPC defines no meaning for a path, so the URL is passed through
	// with only its scheme and host consulted.
	if !httpProtocol {
		return exportTarget{url: raw}, nil
	}

	// OTLP/HTTP: the base endpoint plus the traces signal path — unless the
	// operator already wrote it.
	//
	// Appending unconditionally is what the specification says to do with a base
	// endpoint, and it is also what every collector's own documentation defeats:
	// the URL those docs print is the signal URL, so pasting it produced
	// .../v1/traces/v1/traces, a 404 per export batch and not one span stored.
	// Nothing serves the traces signal at a path ending in the signal path
	// twice, so treating an endpoint that already ends in it as complete cannot
	// mistake a working collector for a mistyped one — while rejecting the value
	// outright would turn one pasted URL into a boot failure.
	if cleaned := path.Clean("/" + u.Path); strings.HasSuffix(cleaned, "/"+tracesSignalPath) {
		u.Path = cleaned
	} else {
		u.Path = path.Join(u.Path, tracesSignalPath)
	}
	return exportTarget{url: u.String()}, nil
}
