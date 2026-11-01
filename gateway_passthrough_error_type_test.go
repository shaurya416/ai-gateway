package aigateway

import (
	"context"
	"net/http"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/pkg/metrics"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// A pass-through forwards the caller's own body verbatim, so a 4xx it relays
// is the upstream's verdict on that body. The target's circuit breaker scores
// it as answered and the error-rate window as a success; the provider-error
// series must not convict the provider for it either, or one client sending
// malformed requests reads as an outage of a healthy upstream. A 5xx is still
// the provider's, and a 429 is counted as it is on every routed surface.
func TestRoutePassthrough_RelayedCallerErrorIsNotAProviderError(t *testing.T) {
	const target = "passthrough-caller-4xx"
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: target}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw.RegisterProvider(&mockProvider{name: target, models: []string{testModel}})

	status := func(code int) func(context.Context) error {
		return func(context.Context) error {
			return &core.HTTPStatusError{StatusCode: code, Message: "upstream answer"}
		}
	}
	providerErrors := metrics.ForProviderError(target, metrics.ErrTypeProvider)

	tests := []struct {
		name  string
		route func() error
		want  float64
	}{
		{
			name: "a 400 relayed on the /v1/* pass-through",
			route: func() error {
				return gw.RoutePassthrough(context.Background(), target, testModel, "hi", true, status(http.StatusBadRequest))
			},
			want: 0,
		},
		{
			name: "a 404 relayed on the /v1/* pass-through",
			route: func() error {
				return gw.RoutePassthrough(context.Background(), target, "", "", true, status(http.StatusNotFound))
			},
			want: 0,
		},
		{
			name: "a 422 relayed on POST /v1/responses",
			route: func() error {
				return gw.RouteResponses(context.Background(), target, testModel, "hi", true, 0, nil, status(http.StatusUnprocessableEntity))
			},
			want: 0,
		},
		{
			name: "a 503 is the provider's",
			route: func() error {
				return gw.RoutePassthrough(context.Background(), target, testModel, "hi", true, status(http.StatusServiceUnavailable))
			},
			want: 1,
		},
		{
			name: "a 429 is counted as the routed surfaces count it",
			route: func() error {
				return gw.RouteResponses(context.Background(), target, testModel, "hi", true, 0, nil, status(http.StatusTooManyRequests))
			},
			want: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := counterValue(t, providerErrors)
			if err := tt.route(); err == nil {
				t.Fatal("expected the relayed status to fail the request")
			}
			if got := counterValue(t, providerErrors) - before; got != tt.want {
				t.Errorf("provider_errors_total{error_type=%q} grew by %v, want %v", metrics.ErrTypeProvider, got, tt.want)
			}
		})
	}
}
