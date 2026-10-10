package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/ferro-labs/ai-gateway/internal/httpclient"
	"github.com/ferro-labs/ai-gateway/internal/transport"
	"github.com/ferro-labs/ai-gateway/internal/version"
	"github.com/ferro-labs/ai-gateway/pkg/logger"
)

const (
	// maxErrorBodyBytes caps how many bytes of a non-2xx MCP response body are
	// read into an error message.
	maxErrorBodyBytes = 4096
	// maxResponseBodyBytes caps the size of a successful MCP JSON-RPC response
	// body. An MCP server is an untrusted-content boundary; without a limit a
	// buggy, compromised, or MITM'd server could return an unbounded body and
	// drive gateway memory exhaustion. The per-request HTTP timeout bounds time,
	// not memory.
	maxResponseBodyBytes = 10 << 20 // 10 MiB
	// maxToolListPages bounds how many tools/list pages one discovery follows,
	// so a server that hands out cursors forever fails its initialization
	// instead of holding the handshake open until it times out. Page size is
	// the server's choice, so the bound sits far above any real listing: a
	// server paging a few hundred tools five at a time is a legitimate
	// configuration, and refusing it loses every tool. Memory is bounded
	// separately, by the listing's total size (see ListTools).
	maxToolListPages = 1024
)

// Client communicates with a single MCP server over Streamable HTTP transport.
// All exported methods are safe for concurrent use.
type Client struct {
	endpoint   string
	headers    map[string]string
	httpClient *http.Client

	// sessionMu protects sessionID. Written during Initialize(), read on every
	// subsequent call. RWMutex ensures concurrent CallTool invocations only
	// contend for a read lock after the session is established.
	sessionMu sync.RWMutex
	sessionID string

	// renewMu serialises session renewal, so concurrent calls that all meet the
	// same expired session start one replacement between them rather than one
	// each.
	renewMu sync.Mutex

	// nextID is incremented atomically to produce unique JSON-RPC request IDs.
	nextID atomic.Int64
}

// NewClient creates an MCP client for the given Streamable HTTP endpoint.
// timeout is the per-request HTTP timeout; 0 defaults to 30 seconds.
func NewClient(endpoint string, headers map[string]string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = defaultToolCallTimeout
	}
	return &Client{
		endpoint:   endpoint,
		headers:    headers,
		httpClient: httpclient.New(timeout),
	}
}

// Initialize performs the MCP initialization handshake (initialize +
// notifications/initialized) and stores the Mcp-Session-Id for subsequent
// requests. Safe to call again — it starts a new session.
func (c *Client) Initialize(ctx context.Context) (*ServerInfo, error) {
	params := map[string]any{
		// The revision this build speaks, taken from the same library constant the
		// stdio transport hands to mark3labs. Hardcoding it here made the two
		// transports negotiate different revisions after a dependency bump, with
		// nothing failing to say so.
		"protocolVersion": mcpgo.LATEST_PROTOCOL_VERSION,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]string{
			"name":    "ferro-ai-gateway",
			"version": version.Short(),
		},
	}

	resp, err := c.call(ctx, mcpMethodInitialize, params)
	if err != nil {
		return nil, fmt.Errorf("mcp initialize: %w", err)
	}

	// The initialize result nests the server's identity under serverInfo, while
	// protocolVersion and capabilities are fields of the result itself. ServerInfo
	// is the gateway's flattened summary of the two levels, not the wire shape, so
	// the wire shape is spelled out here — decoding the envelope straight into it
	// read a top-level "name" no server sends, and left every HTTP server nameless
	// while stdio servers reported theirs.
	var result struct {
		ProtocolVersion string       `json:"protocolVersion"`
		Capabilities    Capabilities `json:"capabilities"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("mcp initialize unmarshal: %w", err)
	}
	info := ServerInfo{
		Name:            result.ServerInfo.Name,
		Version:         result.ServerInfo.Version,
		ProtocolVersion: result.ProtocolVersion,
		Capabilities:    result.Capabilities,
	}

	// Warn and keep going, never reject. A server naming a revision this build
	// does not list is usually newer than the pinned library, and the parts the
	// gateway uses — tools/list and tools/call — have been stable across every
	// revision so far. Refusing here would take working deployments offline on a
	// dependency bump; the stdio transport's stricter behaviour comes from the
	// library, not from a policy chosen here.
	if !slices.Contains(mcpgo.ValidProtocolVersions, info.ProtocolVersion) {
		logger.Default().Warn("mcp server negotiated an unrecognised protocol version; continuing",
			"endpoint", c.endpoint,
			"protocol_version", info.ProtocolVersion,
			"known_versions", mcpgo.ValidProtocolVersions)
	}

	// Send the initialized notification. A refusal is non-fatal — a server that
	// does not gate on it is still usable — but it is reported: one that does
	// gate answers every request after it with an error that names nothing, and
	// this line is what names the cause.
	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
		logger.Default().Warn("mcp server did not accept notifications/initialized; continuing",
			"endpoint", c.endpoint,
			"error", err)
	}

	return &info, nil
}

// ListTools retrieves the full list of tools from the MCP server.
//
// tools/list is paginated: a page that carries a nextCursor has more after it,
// fetched by sending that cursor back. Reading only the first page dropped every
// tool past it with nothing to say so — the server reported ready, an
// allowed_tools entry naming a later tool matched nothing, and the stdio
// transport, whose library follows the cursor, discovered the same server's
// tools in full.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var (
		tools  []Tool
		params any
		listed int
	)
	for range maxToolListPages {
		resp, err := c.call(ctx, mcpMethodToolsList, params)
		if err != nil {
			return nil, fmt.Errorf("mcp tools/list: %w", err)
		}
		// Each page is capped by maxResponseBodyBytes; the listing as a whole is
		// held to the same bound, so following cursors cannot hold more tool
		// definitions in memory than one unpaginated answer could.
		listed += len(resp.Result)
		if listed > maxResponseBodyBytes {
			return nil, fmt.Errorf("mcp tools/list: listing exceeds %d byte limit", maxResponseBodyBytes)
		}

		var result struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return nil, fmt.Errorf("mcp tools/list unmarshal: %w", err)
		}
		tools = append(tools, result.Tools...)
		if result.NextCursor == "" {
			return tools, nil
		}
		params = map[string]string{"cursor": result.NextCursor}
	}
	return nil, fmt.Errorf("mcp tools/list: server returned more than %d pages", maxToolListPages)
}

// CallTool invokes a named tool on the MCP server with the given JSON-encoded
// arguments. Safe for concurrent use from multiple goroutines.
func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (*ToolCallResult, error) {
	params := map[string]any{
		"name":      name,
		"arguments": arguments,
	}

	resp, err := c.call(ctx, mcpMethodToolsCall, params)
	if err != nil {
		return nil, fmt.Errorf("mcp tools/call %s: %w", name, err)
	}

	var result ToolCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("mcp tools/call %s unmarshal: %w", name, err)
	}
	return &result, nil
}

// errSessionExpired reports a 404 to a request that carried a session ID: the
// server has terminated that session and no longer recognises it.
var errSessionExpired = errors.New("mcp server no longer recognises the session")

// call sends a JSON-RPC 2.0 request and returns the decoded response.
// It sets all required headers including the session ID once established.
//
// A server that terminates a session answers every later request carrying it
// with 404, and the spec obliges the client to start a new session when it sees
// one. Without that, a server restart — which drops every in-memory session —
// left the gateway presenting the dead session ID for the rest of the process:
// each tool call failed with 404 while the server was up and answering, and
// nothing short of a configuration reload recovered it. The request is retried
// once on the new session; a 404 means the server never processed it, so the
// retry cannot repeat a tool's side effects.
func (c *Client) call(ctx context.Context, method string, params any) (*JSONRPCResponse, error) {
	// An InitializeRequest starts a session, so it never carries one.
	if method == mcpMethodInitialize {
		return c.send(ctx, method, params, "")
	}
	sid := c.getSessionID()
	resp, err := c.send(ctx, method, params, sid)
	if !errors.Is(err, errSessionExpired) {
		return resp, err
	}
	if renewErr := c.renewSession(ctx, sid); renewErr != nil {
		return nil, fmt.Errorf("%w; starting a new session failed: %w", err, renewErr)
	}
	return c.send(ctx, method, params, c.getSessionID())
}

// renewSession replaces the expired session stale with a new one, unless a
// concurrent call has already done so.
//
// The stale ID is kept until a handshake succeeds, so a renewal that fails
// leaves the next call to meet the same 404 and try again, rather than sending
// no session at all — which a session-requiring server refuses with a 400 that
// never triggers a renewal.
func (c *Client) renewSession(ctx context.Context, stale string) error {
	c.renewMu.Lock()
	defer c.renewMu.Unlock()
	if c.getSessionID() != stale {
		return nil
	}
	if _, err := c.Initialize(ctx); err != nil {
		return err
	}
	// A server that issued no ID this time runs without sessions now, and the
	// stale one must not keep being presented to it.
	c.sessionMu.Lock()
	if c.sessionID == stale {
		c.sessionID = ""
	}
	c.sessionMu.Unlock()
	logger.Default().Info("mcp server no longer recognised the session; started a new one",
		"endpoint", c.endpoint)
	return nil
}

// send performs one JSON-RPC exchange, attaching sid as the session ID when it
// is non-empty.
func (c *Client) send(ctx context.Context, method string, params any, sid string) (*JSONRPCResponse, error) {
	id := c.nextID.Add(1)

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("mcp marshal params: %w", err)
		}
		rawParams = b
	}

	rpcReq := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  rawParams,
	}
	body, err := json.Marshal(rpcReq)
	if err != nil {
		return nil, fmt.Errorf("mcp marshal request: %w", err)
	}

	httpReq, err := c.newPost(ctx, body, sid)
	if err != nil {
		return nil, fmt.Errorf("mcp new http request: %w", err)
	}

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("mcp http do: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	// Checked before the session header is read: a refusal names a session that
	// is over, and adopting one echoed on it would undo the renewal it triggers.
	if httpResp.StatusCode == http.StatusNotFound && sid != "" {
		return nil, fmt.Errorf("mcp server %s returned HTTP %d: %w", method, httpResp.StatusCode, errSessionExpired)
	}

	// Persist the session ID returned by the server (set on initialize).
	c.setSessionID(httpResp.Header.Get("Mcp-Session-Id"))

	if httpResp.StatusCode != http.StatusOK {
		// A refused redirect has no body worth quoting, and the endpoint is
		// POSTed verbatim — a server that answers /mcp with a 307 to /mcp/ is
		// the likeliest 3xx here, and reads identically to a cross-host hop
		// unless the target is named.
		if hint := transport.RedirectTarget(httpResp); hint != "" {
			return nil, fmt.Errorf("mcp server %s returned HTTP %d: %s", method, httpResp.StatusCode, hint)
		}
		errBody, _ := io.ReadAll(io.LimitReader(httpResp.Body, maxErrorBodyBytes))
		return nil, fmt.Errorf("mcp server %s returned HTTP %d: %s", method, httpResp.StatusCode, errBody)
	}

	// Bound the success-path read: read one byte past the cap so an
	// over-limit body is detected rather than silently truncated.
	respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, maxResponseBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("mcp read body: %w", err)
	}
	if len(respBody) > maxResponseBodyBytes {
		return nil, fmt.Errorf("mcp response from %s exceeds %d byte limit", method, maxResponseBodyBytes)
	}

	rpcResp, err := decodeRPCResponse(httpResp.Header.Get("Content-Type"), respBody)
	if err != nil {
		return nil, err
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("mcp rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp, nil
}

// decodeRPCResponse decodes the response to a single JSON-RPC request from a
// body that is either a JSON object or an SSE stream.
//
// Streamable HTTP lets a server answer a POSTed request either way, and the
// Accept header above offers both, so both must be read: a spec-conformant
// server that chose SSE would otherwise never get past the handshake.
//
// The stream carries the response to this request, possibly preceded by
// requests and notifications the server initiated. Those answer nothing the
// gateway asked, so the first EVENT carrying a result or an error is the one to
// take. Anything else — a keep-alive comment, an event whose data is not JSON —
// is skipped rather than treated as a protocol failure.
func decodeRPCResponse(contentType string, body []byte) (*JSONRPCResponse, error) {
	if !isEventStream(contentType) {
		var rpcResp JSONRPCResponse
		if err := json.Unmarshal(body, &rpcResp); err != nil {
			return nil, fmt.Errorf("mcp response unmarshal: %w", err)
		}
		return &rpcResp, nil
	}
	if resp := decodeSSEResponse(body); resp != nil {
		return resp, nil
	}
	return nil, errors.New("mcp sse response carried no result")
}

// decodeSSEResponse returns the first SSE event in body whose data decodes to a
// JSON-RPC result or error, or nil when the stream carries none.
//
// It parses the already-buffered body directly instead of reaching for
// core.SSEDataLines, for two reasons the shared helper cannot serve here:
//
//   - That helper sizes a bufio.Scanner for a LIVE provider stream and caps a
//     line at 1 MiB, while the body read above is capped at 10 MiB. An MCP
//     result is one JSON document on one line — an embedded image or a large
//     file read is routinely megabytes — so the two caps disagreed, and the same
//     tool result succeeded as JSON and failed as SSE at 1 MiB. Raising the
//     shared cap is not the fix: six provider streaming paths share it, and it
//     is their per-stream memory bound against an untrusted upstream. Here the
//     whole body is already in memory and already bounded, so a second, smaller
//     bound buys nothing.
//   - Per the SSE spec an event's data field may span several data: lines, which
//     are joined with newlines before dispatch. Decoding each line on its own
//     rejected a split response entirely.
func decodeSSEResponse(body []byte) *JSONRPCResponse {
	var data []string

	// dispatch decodes the accumulated data field of one event and clears it.
	dispatch := func() *JSONRPCResponse {
		joined := strings.Join(data, "\n")
		data = data[:0]
		var rpcResp JSONRPCResponse
		if err := json.Unmarshal([]byte(joined), &rpcResp); err != nil {
			return nil
		}
		if rpcResp.Result == nil && rpcResp.Error == nil {
			return nil
		}
		return &rpcResp
	}

	for raw := range strings.Lines(string(body)) {
		line := strings.TrimRight(raw, "\r\n")
		if line == "" { // a blank line dispatches the event
			if resp := dispatch(); resp != nil {
				return resp
			}
			continue
		}
		value, ok := strings.CutPrefix(line, "data:")
		if !ok { // a comment, or a field the gateway does not use
			continue
		}
		// The SSE spec strips a single optional space after the colon.
		data = append(data, strings.TrimPrefix(value, " "))
	}
	// The spec discards an event that no blank line terminated. Dispatch it
	// anyway: a server that ends its body straight after the last data: line has
	// still delivered the answer, and the stricter reading would fail the call.
	if len(data) > 0 {
		return dispatch()
	}
	return nil
}

// isEventStream reports whether a Content-Type names the SSE media type,
// ignoring any parameters (servers commonly append charset).
func isEventStream(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "text/event-stream"
}

// notify sends a JSON-RPC 2.0 notification (no ID, no JSON-RPC response) and
// reports an error when the server does not accept it.
func (c *Client) notify(ctx context.Context, method string, params any) error {
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		rawParams = b
	}

	notification := struct {
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{
		JSONRPC: "2.0",
		Method:  method,
		Params:  rawParams,
	}
	body, err := json.Marshal(notification)
	if err != nil {
		return err
	}

	httpReq, err := c.newPost(ctx, body, c.getSessionID())
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	// A server that accepts a notification answers 202 and one that cannot
	// MUST answer with an error status, so the status is the delivery receipt.
	// Discarding it is how a refused notification read as a sent one.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if hint := transport.RedirectTarget(resp); hint != "" {
			return fmt.Errorf("mcp server refused %s: HTTP %d: %s", method, resp.StatusCode, hint)
		}
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return fmt.Errorf("mcp server refused %s: HTTP %d: %s", method, resp.StatusCode, errBody)
	}
	return nil
}

// newPost builds one POST of body to the endpoint, carrying the headers the
// transport requires on every message plus the operator's own. sid is attached
// as the session ID when it is non-empty.
//
// Requests and notifications share it so neither can drift from the other. The
// spec requires every POST to offer both response media types, and the
// notification path, which built its request separately, sent no Accept at all:
// the official Python SDK refuses that 406 and the Go SDK 400, so
// notifications/initialized never reached the server. One that gates requests on
// it — the Python SDK before 1.18 — then answered every tools/list with an
// error, and the server never became ready.
func (c *Client) newPost(ctx context.Context, body []byte, sid string) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if sid != "" {
		httpReq.Header.Set("Mcp-Session-Id", sid)
	}
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}
	return httpReq, nil
}

// getSessionID reads the current session ID under a read lock.
func (c *Client) getSessionID() string {
	c.sessionMu.RLock()
	defer c.sessionMu.RUnlock()
	return c.sessionID
}

// setSessionID writes the session ID under a write lock. Empty strings are ignored.
func (c *Client) setSessionID(sid string) {
	if sid == "" {
		return
	}
	c.sessionMu.Lock()
	c.sessionID = sid
	c.sessionMu.Unlock()
}

// sessionCloseTimeout bounds the DELETE that ends a session on Close. Teardown
// shares Gateway.Close's few seconds with every other server, and a session the
// server never hears about ends on its own schedule anyway.
const sessionCloseTimeout = 2 * time.Second

// Close ends the server-side session, when the server issued one, with the
// DELETE the transport defines for a client that no longer needs it. A server
// that does not let clients end sessions answers 405, and one that already
// ended it answers 404; neither is a failure.
//
// Without it every retired client left its session open on the server: one per
// HTTP server on every configuration reload and every restart. By default a
// server built on the official Python SDK before 2.0 keeps such a session, and
// the task serving it, until the server itself restarts; later ones hold it for
// half an hour, counted against their session limit.
//
// Safe to call more than once; only the first call sends anything.
func (c *Client) Close() error {
	c.sessionMu.Lock()
	sid := c.sessionID
	c.sessionID = ""
	c.sessionMu.Unlock()
	if sid == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), sessionCloseTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.endpoint, nil)
	if err != nil {
		return fmt.Errorf("mcp end session: %w", err)
	}
	req.Header.Set("Mcp-Session-Id", sid)
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("mcp end session: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299,
		resp.StatusCode == http.StatusNotFound,
		resp.StatusCode == http.StatusMethodNotAllowed:
		return nil
	}
	if hint := transport.RedirectTarget(resp); hint != "" {
		return fmt.Errorf("mcp server refused to end the session: HTTP %d: %s", resp.StatusCode, hint)
	}
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	return fmt.Errorf("mcp server refused to end the session: HTTP %d: %s", resp.StatusCode, errBody)
}
