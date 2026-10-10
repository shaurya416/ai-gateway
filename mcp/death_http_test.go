package mcp

import (
	"context"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestHTTPBrokenPipeKeepsServerReady pins the documented boundary of death
// detection: an HTTP server is never withdrawn after its handshake.
//
// net/http reports a connection the server reset mid-upload — a server
// restarting, or a proxy refusing a large body — as a write that failed with
// EPIPE in a share of cases, depending on which of its loops sees the reset
// first, and as ECONNRESET in the rest. EPIPE is conclusive for a stdio pipe,
// so the executor withdrew the HTTP server on it: its tools stopped being
// advertised and, with nothing ever re-initializing an HTTP server, stayed
// withdrawn until the next configuration reload, though the next request would
// have opened a new connection and been served.
func TestHTTPBrokenPipeKeepsServerReady(t *testing.T) {
	var sent atomic.Int32
	c := NewClient("http://mcp.invalid/mcp", nil, 5*time.Second)
	c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		sent.Add(1)
		// The shape net/http returns for that reset: the socket write failed.
		return nil, &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)}
	})

	// The failure must be one the stdio rule reads as a dead transport, or this
	// test proves nothing about it.
	if _, err := c.CallTool(context.Background(), "t1", nil); !isTransportDead(err) {
		t.Fatalf("precondition: CallTool error %v is not a dead-transport error", err)
	}

	reg := registryWith(map[string]mcpClient{"remote": c})
	reg.mu.Lock()
	reg.servers["remote"].tools = []Tool{{Name: "t1"}}
	reg.toolMap["t1"] = "remote"
	reg.mu.Unlock()

	exec := NewExecutor(reg, 5, nil)
	before := sent.Load()
	exec.executeToolCall(context.Background(), toolCallNamed("t1"))

	if !reg.IsReady("remote") {
		t.Fatal("one broken connection withdrew an HTTP server")
	}
	if len(reg.AllTools()) != 1 {
		t.Errorf("AllTools = %v after one broken connection; want the server's tool still advertised", reg.AllTools())
	}
	exec.executeToolCall(context.Background(), toolCallNamed("t1"))
	if got := sent.Load() - before; got != 2 {
		t.Errorf("the server was sent %d calls; want the call after the broken connection sent as well", got)
	}
}
