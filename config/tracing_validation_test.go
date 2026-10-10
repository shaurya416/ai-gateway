package config_test

import (
	"math"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
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
