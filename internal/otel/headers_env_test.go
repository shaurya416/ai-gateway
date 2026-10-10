package otel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/observability"
)

// observability.tracing.headers and OTEL_EXPORTER_OTLP_HEADERS do not combine:
// a configured header map is handed to the SDK as its header option, which
// replaces whatever the SDK read from the environment, so the variable reaches
// the collector only when no configured header resolves. A deployment that
// keeps its API key in the variable and a project header in config exports
// without the key. This pins that behaviour, which the configuration docs
// state, so the two cannot drift apart again.
func TestConfiguredHeadersReplaceEnvHeaders(t *testing.T) {
	cases := []struct {
		name       string
		configured map[string]string
		wantEnv    bool
		wantConfig bool
	}{
		{name: "no headers configured", configured: nil, wantEnv: true},
		{name: "headers configured", configured: map[string]string{"X-Config-Header": "from-config"}, wantConfig: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu   sync.Mutex
				seen []http.Header
			)
			collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen = append(seen, r.Header.Clone())
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
			}))
			defer collector.Close()

			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
			t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "X-Env-Header=from-env")

			cfg := DefaultConfig()
			cfg.Endpoint = collector.URL
			cfg.Protocol = "http/protobuf"
			cfg.ShutdownGrace = 5 * time.Second
			cfg.Headers = tc.configured

			prov, shutdown, err := Init(context.Background(), cfg)
			if err != nil {
				t.Fatalf("Init: %v", err)
			}
			_, span := prov.StartRequestSpan(context.Background(), observability.RequestAttrs{System: "stub", Operation: "chat"})
			span.End()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = shutdown(shutdownCtx)

			mu.Lock()
			defer mu.Unlock()
			if len(seen) == 0 {
				t.Fatal("the collector was never contacted; the test proved nothing")
			}
			for _, h := range seen {
				if got := h.Get("X-Env-Header") != ""; got != tc.wantEnv {
					t.Errorf("OTEL_EXPORTER_OTLP_HEADERS reached the collector = %v, want %v", got, tc.wantEnv)
				}
				if got := h.Get("X-Config-Header") != ""; got != tc.wantConfig {
					t.Errorf("configured header reached the collector = %v, want %v", got, tc.wantConfig)
				}
			}
		})
	}
}
