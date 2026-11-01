package httpserver_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ferro-labs/ai-gateway/internal/httpserver"
	"github.com/ferro-labs/ai-gateway/internal/middleware"
	"github.com/ferro-labs/ai-gateway/pkg/ratelimit"
)

// TestRealIP_TrustedPeer_XFFHopWithPort covers a trusted proxy that appends the
// address it observed together with that connection's source port, as Azure
// Application Gateway does ("client-ip:port"). That hop is the one the proxy
// vouches for, so it is the client. Read as malformed and stepped over, the
// walk carried on into the entries the caller wrote, and then to the caller's
// own X-Real-IP — handing the caller the address it chose.
func TestRealIP_TrustedPeer_XFFHopWithPort(t *testing.T) {
	tests := []struct {
		name    string
		trusted string
		peer    string
		xff     string
		realIP  string
		want    string
	}{
		{
			name: "proxy hop with port is the client",
			peer: "127.0.0.1:56789",
			xff:  "203.0.113.7:51234",
			want: "203.0.113.7",
		},
		{
			name: "forged leading entry ignored behind a port-writing proxy",
			peer: "127.0.0.1:56789",
			xff:  "9.9.9.9, 203.0.113.7:51234",
			want: "203.0.113.7",
		},
		{
			name:   "caller's X-Real-IP ignored behind a port-writing proxy",
			peer:   "127.0.0.1:56789",
			xff:    "203.0.113.7:51234",
			realIP: "9.9.9.9",
			want:   "203.0.113.7",
		},
		{
			name: "bracketed IPv6 hop with port",
			peer: "127.0.0.1:56789",
			xff:  "2001:db8::9, [2001:db8::7]:51234",
			want: "2001:db8::7",
		},
		{
			name:    "trusted hop with port is stepped over",
			trusted: "10.0.0.0/8",
			peer:    "10.0.0.5:443",
			xff:     "9.9.9.9, 203.0.113.7:51234, 10.0.0.9:443",
			want:    "203.0.113.7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			req.RemoteAddr = tt.peer
			req.Header.Set("X-Forwarded-For", tt.xff)
			if tt.realIP != "" {
				req.Header.Set("X-Real-IP", tt.realIP)
			}

			if got := applyRealIP(t, tt.trusted, req); got != tt.want {
				t.Errorf("XFF %q (X-Real-IP %q): resolved %q, want %q", tt.xff, tt.realIP, got, tt.want)
			}
		})
	}
}

// TestRateLimit_PortWritingProxyKeysOnTheObservedClient is the consequence that
// matters: behind a proxy that writes "ip:port", a caller cannot take a fresh
// per-IP bucket on every request by changing the X-Forwarded-For it sends.
func TestRateLimit_PortWritingProxyKeysOnTheObservedClient(t *testing.T) {
	nets, err := httpserver.ParseTrustedProxyCIDRs("")
	if err != nil {
		t.Fatalf("ParseTrustedProxyCIDRs: %v", err)
	}
	store := ratelimit.NewStore(1, 1) // burst=1: the first request spends the only token
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := httpserver.RealIPMiddleware(nets)(middleware.RateLimit(store)(ok))

	codes := make([]int, 0, 2)
	for i, forged := range []string{"198.51.100.1", "198.51.100.2"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", nil)
		req.RemoteAddr = "127.0.0.1:56789"
		// The caller's own entry, then the one the proxy appended: the same
		// client from a new source port each time.
		req.Header.Set("X-Forwarded-For", forged+", 203.0.113.7:"+[]string{"51234", "51235"}[i])
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
	}

	if codes[0] != http.StatusOK || codes[1] != http.StatusTooManyRequests {
		t.Fatalf("status codes = %v, want [200 429]: a forged X-Forwarded-For entry bought a fresh bucket", codes)
	}
}
