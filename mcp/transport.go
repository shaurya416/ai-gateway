package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"syscall"

	"github.com/mark3labs/mcp-go/client/transport"
)

// mcpClient is the internal interface all MCP transport adapters must satisfy.
// Both the hand-rolled HTTP client (*Client) and the stdio adapter (*stdioClient)
// implement this interface so the Registry is transport-agnostic.
type mcpClient interface {
	// Initialize performs the MCP initialization handshake with the server.
	Initialize(ctx context.Context) (*ServerInfo, error)
	// ListTools retrieves the full list of tools advertised by the server.
	ListTools(ctx context.Context) ([]Tool, error)
	// CallTool invokes a named tool with JSON-encoded arguments.
	CallTool(ctx context.Context, name string, arguments json.RawMessage) (*ToolCallResult, error)
	// Close releases any resources held by the transport (e.g. subprocess, connection).
	Close() error
}

// pinger is the optional confirmation probe a transport may offer. MCP makes
// ping mandatory for servers, so a transport that can issue one can distinguish
// a server that has genuinely gone from one that merely looks that way.
//
// Optional rather than part of mcpClient: only the stdio transport has a death
// signal to confirm, and widening the interface would oblige every adapter to
// carry a method nothing calls on it.
type pinger interface {
	Ping(ctx context.Context) error
}

// isTransportDead reports whether err is conclusive evidence that the server on
// the other end of the transport is gone, as opposed to one call going wrong.
//
// Two errors qualify. ErrTransportClosed means the transport's own reader saw
// stdout EOF and closed itself. EPIPE (equivalently io.ErrClosedPipe) means a
// write reached a pipe with no reader left — the case where a descendant still
// holds the pipes open, so the transport never noticed and reports the raw
// syscall error instead.
//
// Nothing else belongs here. A timeout, a refused connection, or a malformed
// frame can all resolve on their own, and withdrawing a server is terminal
// until the next configuration reload.
//
// Both readings are about a stdio pipe. An HTTP client's error can carry EPIPE
// too, and means less there — see withdrawsOnDeadTransport.
func isTransportDead(err error) bool {
	return errors.Is(err, transport.ErrTransportClosed) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.ErrClosedPipe)
}

// withdrawsOnDeadTransport reports whether a dead transport is grounds for
// withdrawing the server behind it.
//
// It is for stdio, whose pipes are the server process: once they break, the
// process has gone. It is not for the HTTP transport. net/http reports a
// connection the server reset mid-upload — a server restarting, or a proxy
// refusing a large body — as a write that failed with EPIPE in a share of
// cases, and that connection is all it speaks for: the next request opens
// another. Nothing re-initializes a withdrawn server, so withdrawing on it took
// the server's tools away until the next configuration reload and, for a
// required server, held /readyz at 503 while the server was answering again.
// Death after the handshake is detected for stdio servers only.
func withdrawsOnDeadTransport(c mcpClient) bool {
	_, isHTTP := c.(*Client)
	return !isHTTP
}
