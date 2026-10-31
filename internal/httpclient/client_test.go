package httpclient

import (
	"net/http"
	"testing"
	"time"
)

func TestForProvider_KnownProvider(t *testing.T) {
	// Known providers (registered via RegisterKnownProviders at init) must get
	// a dedicated pool — not the shared default client.
	oai := ForProvider("openai")
	if oai == nil {
		t.Fatal("ForProvider(\"openai\") must not be nil")
	}
	if oai == manager.DefaultClient() {
		t.Fatal("ForProvider(\"openai\") must return a dedicated client, not the default client")
	}
}

func TestForProvider_UnknownProvider(t *testing.T) {
	// Unknown providers must fall back to the shared default client.
	unknown := ForProvider("some-unknown-provider")
	if unknown != manager.DefaultClient() {
		t.Fatal("ForProvider(unknown) must return the default client")
	}
}

func TestNew_WithTimeout(t *testing.T) {
	client := New(5 * time.Second)
	if client.Timeout != 5*time.Second {
		t.Errorf("timeout = %v, want 5s", client.Timeout)
	}
	if client.Transport != manager.DefaultTransport() {
		t.Error("New(timeout) must reuse the shared transport")
	}
}

func TestNew_ZeroTimeout(t *testing.T) {
	if New(0) != manager.DefaultClient() {
		t.Fatal("New(0) must return the default client")
	}
}

func TestCloseIdleConnections_NoPanic(_ *testing.T) {
	// Must not panic even when called multiple times.
	CloseIdleConnections()
	CloseIdleConnections()
}

// A timeout is the bound on the whole exchange. The shared transport gives up
// waiting for response headers after its own bound, so a client asking for
// longer than that must not be handed a transport that cuts it short there.
func TestNew_TimeoutLongerThanTheHeaderBoundIsHonoured(t *testing.T) {
	bound := manager.DefaultTransport().ResponseHeaderTimeout
	if bound <= 0 {
		t.Skip("the shared transport sets no response-header bound")
	}
	for _, timeout := range []time.Duration{time.Second, bound, bound + time.Second, 30 * time.Minute} {
		client := New(timeout)
		if client.Timeout != timeout {
			t.Errorf("New(%v): timeout = %v", timeout, client.Timeout)
		}
		rt, ok := client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("New(%v): transport is %T", timeout, client.Transport)
		}
		if got := rt.ResponseHeaderTimeout; got > 0 && got < timeout {
			t.Errorf("New(%v): transport stops waiting for headers after %v", timeout, got)
		}
	}
}
