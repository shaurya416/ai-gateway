package config_test

import (
	"math"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	gwotel "github.com/ferro-labs/ai-gateway/internal/otel"
)

// TestValidateConfig_TracingProtocolAndSampleRatio is the regression test for
// two tracing settings that loaded whatever they held and then did something
// other than what was written.
//
// An unrecognised protocol — "http/json", "HTTP/protobuf" — silently selected
// gRPC, so every export to the HTTP collector it named failed. A sample_ratio
// outside 0.0–1.0 was clamped: 10, meaning ten percent, sampled every trace,
// and a negative ratio sampled none while tracing reported itself enabled.
func TestValidateConfig_TracingProtocolAndSampleRatio(t *testing.T) {
	ratio := func(v float64) *float64 { return &v }
	tests := []struct {
		name    string
		tracing config.TracingConfig
		wantErr string
	}{
		{name: "defaults", tracing: config.TracingConfig{}},
		{name: "grpc", tracing: config.TracingConfig{Protocol: "grpc"}},
		{name: "http/protobuf", tracing: config.TracingConfig{Protocol: "http/protobuf"}},
		{name: "ratio zero", tracing: config.TracingConfig{SampleRatio: ratio(0)}},
		{name: "ratio one", tracing: config.TracingConfig{SampleRatio: ratio(1)}},
		{name: "ratio fraction", tracing: config.TracingConfig{SampleRatio: ratio(0.25)}},
		{name: "unsupported protocol", tracing: config.TracingConfig{Protocol: "http/json"}, wantErr: "http/json"},
		{name: "protocol in the wrong case", tracing: config.TracingConfig{Protocol: "HTTP/protobuf"}, wantErr: "HTTP/protobuf"},
		{name: "ratio written as a percentage", tracing: config.TracingConfig{SampleRatio: ratio(10)}, wantErr: "sample_ratio"},
		{name: "negative ratio", tracing: config.TracingConfig{SampleRatio: ratio(-0.5)}, wantErr: "sample_ratio"},
		{name: "NaN ratio", tracing: config.TracingConfig{SampleRatio: ratio(math.NaN())}, wantErr: "sample_ratio"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{
				Strategy:      config.StrategyConfig{Mode: config.ModeSingle},
				Targets:       []config.Target{{VirtualKey: "key1"}},
				Observability: config.ObservabilityConfig{Tracing: tc.tracing},
			}
			err := config.ValidateConfig(cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateConfig: unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateConfig accepted %+v", tc.tracing)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q should name %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateConfig_TracingEndpoint holds `ferrogw validate` to the answer
// startup gives for observability.tracing.endpoint.
//
// The tracing backend refuses an endpoint it cannot export to when it starts —
// a scheme-less value carrying a path, a scheme other than http or https, a URL
// with no host — and the gateway exits. Config validation did not check the
// field at all, so `ferrogw validate` reported such a config valid and the
// deploy it approved never came up.
func TestValidateConfig_TracingEndpoint(t *testing.T) {
	tests := []struct {
		endpoint string
		wantErr  bool
	}{
		{endpoint: ""},
		{endpoint: "localhost:4317"},
		{endpoint: "jaeger:4317"},
		{endpoint: "http://collector:4318"},
		{endpoint: "https://collector.example.com:4318/v1/traces"},
		{endpoint: "otel-collector:4318/v1/traces", wantErr: true},
		{endpoint: "grpc://collector:4317", wantErr: true},
		{endpoint: "http://", wantErr: true},
		{endpoint: "http://coll ector:4318", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.endpoint, func(t *testing.T) {
			cfg := config.Config{
				Strategy:      config.StrategyConfig{Mode: config.ModeSingle},
				Targets:       []config.Target{{VirtualKey: "key1"}},
				Observability: config.ObservabilityConfig{Tracing: config.TracingConfig{Endpoint: tc.endpoint}},
			}
			loadErr := config.ValidateConfig(cfg)
			if (loadErr != nil) != tc.wantErr {
				t.Fatalf("ValidateConfig(endpoint %q) = %v, want error: %v", tc.endpoint, loadErr, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(loadErr.Error(), "endpoint") {
				t.Errorf("error %q should name the endpoint", loadErr)
			}

			// What startup's tracing validator says about the same value.
			startErr := gwotel.Config{Enabled: true, Protocol: "http/protobuf", Endpoint: tc.endpoint}.Validate()
			if (startErr != nil) != (loadErr != nil) {
				t.Errorf("config validation and startup disagree on endpoint %q: load %v, startup %v", tc.endpoint, loadErr, startErr)
			}
		})
	}
}
