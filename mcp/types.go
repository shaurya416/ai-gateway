// Package mcp implements the Model Context Protocol (MCP) 2025-11-25
// Streamable HTTP transport for the Ferro Labs AI Gateway.
//
// It defines the public configuration types (ServerConfig, ToolCallAuditFn) that
// callers embed in the gateway config, and provides a thread-safe client, a
// concurrent-safe server registry, and an agentic tool-call loop executor that
// integrates with gateway.Route.
package mcp

import "encoding/json"

// MCP protocol method names used in JSON-RPC calls.
const (
	mcpMethodInitialize = "initialize"
	mcpMethodToolsList  = "tools/list"
	mcpMethodToolsCall  = "tools/call"
)

// ─── JSON-RPC 2.0 ────────────────────────────────────────────────────────────

// JSONRPCRequest is a JSON-RPC 2.0 request envelope.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JSONRPCResponse is a JSON-RPC 2.0 response envelope.
type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`

	// sessionID is the Mcp-Session-Id header the HTTP response carried, empty
	// when it carried none. Initialize reads it to decide the session.
	sessionID string
}

// JSONRPCError is the error object nested inside a failed JSON-RPC 2.0 response.
type JSONRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// ─── MCP Protocol Types ───────────────────────────────────────────────────────

// Tool represents an MCP tool definition as returned by tools/list.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ToolCallResult holds the result of a single tools/call invocation.
type ToolCallResult struct {
	Content []ContentBlock `json:"content"`
	// StructuredContent is the tool's answer as a JSON object, for a tool that
	// declares an output schema. The spec only asks such a tool to repeat it as
	// a text block, so it may be the whole answer. Kept raw, as the server wrote
	// it.
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// ContentBlock is a single piece of content returned by a tool call.
// Type is one of "text", "image", "audio", "resource_link", or "resource"
// (MCP 2025-11-25).
//
// Phase 1 only extracts the text payload for conversation messages;
// non-text fields are decoded and preserved but not converted to prose.
type ContentBlock struct {
	Type string `json:"type"`
	// Text carries the content for type="text" blocks.
	Text string `json:"text,omitempty"`
	// Data and MimeType are populated for type="image" and type="audio"
	// blocks. Data is a base64-encoded payload; MimeType is e.g. "image/png".
	// A resource_link block carries MimeType as well.
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	// URI, Name, Title, Description and Size describe the resource a
	// type="resource_link" block points at. The URI is the tool's answer, so a
	// block that dropped these fields would reach the model as a link to
	// nothing. Size keeps the number as the server wrote it: it is metadata
	// the model reads, and a server writing 1024.0 must not fail the result.
	URI         string      `json:"uri,omitempty"`
	Name        string      `json:"name,omitempty"`
	Title       string      `json:"title,omitempty"`
	Description string      `json:"description,omitempty"`
	Size        json.Number `json:"size,omitempty"`
	// Resource holds the embedded resource object for type="resource" blocks.
	// Stored as raw JSON for forward compatibility with future MCP spec revisions.
	Resource json.RawMessage `json:"resource,omitempty"`
}

// ServerInfo describes the MCP server returned during the initialize handshake.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// ProtocolVersion is the MCP revision the server negotiated. It is a
	// top-level field of the initialize result, not part of the server's own
	// identity, and it is what the client checks against the revisions this
	// build understands.
	ProtocolVersion string       `json:"protocolVersion,omitempty"`
	Capabilities    Capabilities `json:"capabilities"`
}

// Capabilities advertised by an MCP server during initialization.
type Capabilities struct {
	Tools     *ToolsCapability     `json:"tools,omitempty"`
	Resources *ResourcesCapability `json:"resources,omitempty"`
	Prompts   *PromptsCapability   `json:"prompts,omitempty"`
}

// ToolsCapability advertises tool-related server capabilities.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ResourcesCapability advertises resource-related server capabilities.
type ResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// PromptsCapability advertises prompt-related server capabilities.
type PromptsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}
