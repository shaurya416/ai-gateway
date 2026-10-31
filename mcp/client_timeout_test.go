package mcp

import (
	"net/http"
	"testing"
	"time"
)

// timeout_seconds bounds a whole tool call, the wait for the answer's headers
// included. A server answering with a JSON body sends no header until the tool
// has finished, so a transport that stops waiting for headers sooner than the
// configured timeout cuts every long call short at its own bound — a call the
// same tool over stdio would have been allowed to finish.
func TestHTTPServerTimeoutIsNotCutShortWaitingForHeaders(t *testing.T) {
	for _, seconds := range []int{0, 10, 300, 1800} {
		reg := NewRegistry(nil)
		reg.RegisterConfig(ServerConfig{Name: "slow", URL: "http://127.0.0.1:1/mcp", TimeoutSeconds: seconds})
		t.Cleanup(func() { _ = reg.Close() })

		reg.mu.RLock()
		client, ok := reg.servers["slow"].client.(*Client)
		reg.mu.RUnlock()
		if !ok {
			t.Fatalf("timeout_seconds %d: server is not served by the HTTP client", seconds)
		}

		want := time.Duration(seconds) * time.Second
		if want <= 0 {
			want = defaultToolCallTimeout
		}
		if got := client.httpClient.Timeout; got != want {
			t.Errorf("timeout_seconds %d: client timeout = %v, want %v", seconds, got, want)
		}
		rt, ok := client.httpClient.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("timeout_seconds %d: transport is %T", seconds, client.httpClient.Transport)
		}
		if bound := rt.ResponseHeaderTimeout; bound > 0 && bound < want {
			t.Errorf("timeout_seconds %d: the transport stops waiting for headers after %v, before the %v the server is allowed",
				seconds, bound, want)
		}
	}
}
