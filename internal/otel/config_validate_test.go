package otel

import (
	"context"
	"math"
	"testing"
)

// TestValidateRejectsUnusableProtocolAndSampleRatio covers the same settings at
// the tracing layer's own validator, which Init runs: a value the exporter
// cannot honour is a startup error rather than a different behaviour.
func TestValidateRejectsUnusableProtocolAndSampleRatio(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"unsupported protocol", func(c *Config) { c.Protocol = "http/json" }},
		{"protocol in the wrong case", func(c *Config) { c.Protocol = "HTTP/protobuf" }},
		{"ratio above one", func(c *Config) { c.SampleRatio = 10 }},
		{"negative ratio", func(c *Config) { c.SampleRatio = -0.5 }},
		{"NaN ratio", func(c *Config) { c.SampleRatio = math.NaN() }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Endpoint = "localhost:4317"
			tc.edit(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate accepted %+v", cfg)
			}
			if _, _, err := Init(context.Background(), cfg); err == nil {
				t.Fatalf("Init accepted %+v", cfg)
			}
		})
	}

	for _, protocol := range []string{"", "grpc", "http/protobuf", "http"} {
		cfg := DefaultConfig()
		cfg.Protocol = protocol
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate rejected protocol %q: %v", protocol, err)
		}
	}
}
