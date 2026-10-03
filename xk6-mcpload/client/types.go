// Package client is mcpload's thin MCP streamable-HTTP wire client.
//
// It deliberately has no k6 dependencies so that it can be unit-tested with
// plain httptest servers. It exposes per-request timings through an Observer
// and never retries on its own.
package client

import (
	"encoding/json"
	"fmt"
	"time"
)

// Protocol identifiers.
const (
	// ProtocolAuto probes with a stateless (2026-07-28) request first and falls
	// back to the stateful initialize handshake.
	ProtocolAuto = "auto"
	// ProtocolStateless is the first stateless protocol revision.
	ProtocolStateless = "2026-07-28"
	// DefaultFallbackVersion is the version requested by initialize when
	// protocol "auto" falls back to the stateful handshake.
	DefaultFallbackVersion = "2025-11-25"
)

// Well-known MCP header names.
const (
	HeaderSessionID       = "Mcp-Session-Id"
	HeaderProtocolVersion = "Mcp-Protocol-Version"
	HeaderMethod          = "Mcp-Method"
	HeaderName            = "Mcp-Name"
)

// _meta keys used by the stateless protocol.
const (
	MetaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	MetaClientInfo         = "io.modelcontextprotocol/clientInfo"
	MetaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
)

// MCP-specific JSON-RPC error codes (2026-07-28).
const (
	CodeHeaderMismatch                    = -32020
	CodeMissingRequiredClientCapabilities = -32021
	CodeUnsupportedProtocolVersion        = -32022
	CodeMethodNotFound                    = -32601
)

// Error types, used verbatim as the error_type metric tag.
const (
	ErrHTTP            = "http"
	ErrJSONRPC         = "jsonrpc"
	ErrToolIsError     = "tool_iserror"
	ErrTimeout         = "timeout"
	ErrSessionNotFound = "session_not_found"
	ErrHeaderMismatch  = "header_mismatch"
	ErrAuth            = "auth"
	// ErrUnsupportedRequest: the server sent a request on a response stream
	// that the client has no answer for (answered with -32601), or any
	// request on a stateless (2026-07-28) stream (not answered).
	ErrUnsupportedRequest = "unsupported_request"
	// ErrCancelled: the client cancelled the call itself (CallOptions.CancelAfter).
	// It is not a server error: such calls are not counted in mcp_errors.
	ErrCancelled = "cancelled"
)

// IsStateless reports whether a protocol version uses the stateless
// (2026-07-28+) transport rules. Protocol versions are ISO dates, so string
// comparison is chronological.
func IsStateless(version string) bool {
	return version != ProtocolAuto && version >= ProtocolStateless
}

// ValidProtocolVersion reports whether v is a protocol revision identifier
// (a YYYY-MM-DD date).
func ValidProtocolVersion(v string) bool {
	if len(v) != len("2006-01-02") {
		return false
	}
	_, err := time.Parse("2006-01-02", v)
	return err == nil
}

// ValidateProtocol checks a Protocol option value: "auto" (or empty, meaning
// "auto") or a YYYY-MM-DD protocol revision.
func ValidateProtocol(p string) error {
	if p == "" || p == ProtocolAuto || ValidProtocolVersion(p) {
		return nil
	}
	return fmt.Errorf("invalid protocol %q: want 'auto' or a protocol revision date like %q or %q",
		p, ProtocolStateless, DefaultFallbackVersion)
}

// ValidateFallbackVersion checks a FallbackVersion option value: empty
// (meaning DefaultFallbackVersion) or a YYYY-MM-DD protocol revision.
func ValidateFallbackVersion(v string) error {
	if v == "" || ValidProtocolVersion(v) {
		return nil
	}
	return fmt.Errorf("invalid fallbackVersion %q: want a protocol revision date like %q", v, DefaultFallbackVersion)
}

// Error is returned by every client operation that fails. Type is one of the
// Err* constants.
type Error struct {
	Type       string
	Message    string
	HTTPStatus int             // 0 when no HTTP response was received
	Code       int             // JSON-RPC error code, when there is one
	Data       json.RawMessage // JSON-RPC error data, when there is one
}

func (e *Error) Error() string {
	switch {
	case e.Code != 0:
		return fmt.Sprintf("mcp %s error (http %d, code %d): %s", e.Type, e.HTTPStatus, e.Code, e.Message)
	case e.HTTPStatus != 0:
		return fmt.Sprintf("mcp %s error (http %d): %s", e.Type, e.HTTPStatus, e.Message)
	default:
		return fmt.Sprintf("mcp %s error: %s", e.Type, e.Message)
	}
}

// Implementation identifies the client (clientInfo).
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Tool is one entry of a tools/list result.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// ToolCall is one element of a parallel call batch.
type ToolCall struct {
	Name string
	Args any
	CallOptions
}

// CallOptions are per-call options of CallToolWith.
type CallOptions struct {
	// CancelAfter > 0 cancels the call when no response arrived within it:
	// see Session.CallToolWith.
	CancelAfter time.Duration
}

// ToolResult is the outcome of a tools/call. Err is set on any failure,
// including a result with isError=true (Err.Type == ErrToolIsError).
type ToolResult struct {
	IsError           bool
	Content           json.RawMessage
	StructuredContent json.RawMessage
	Duration          time.Duration
	Err               *Error
	// Cancelled is set when CallOptions.CancelAfter cancelled the call
	// (Err.Type == ErrCancelled).
	Cancelled bool
}

// RequestStats describes one HTTP exchange performed by the client.
type RequestStats struct {
	Method    string // JSON-RPC method, or "DELETE" for session termination
	Tool      string // tool name for tools/call, else ""
	Protocol  string
	Status    int    // HTTP status; 0 when no response was received
	ErrorType string // "" on success
	Start     time.Time
	TTFB      time.Duration
	Duration  time.Duration // full round trip, until the matching response was parsed
	Streamed  bool          // response was text/event-stream
	Stream    time.Duration // time from response headers to the matching SSE event
	NotSent   bool          // request was never sent (e.g. token fetch failed); no timings
}

// ConnectStats describes a connect() handshake as a whole.
type ConnectStats struct {
	Method    string // "initialize", "server/discover" or "" when no call was needed
	Protocol  string
	Status    int
	ErrorType string
	Start     time.Time
	Duration  time.Duration
}

// TokenStats describes one OAuth token fetch.
type TokenStats struct {
	Status    int
	ErrorType string
	Start     time.Time
	Duration  time.Duration
}

// Observer receives timing events. Implementations must be safe for
// concurrent use: CallParallel invokes them from several goroutines.
type Observer interface {
	OnRequest(RequestStats)
	OnConnect(ConnectStats)
	OnTokenFetch(TokenStats)
	OnSessionOpen()
	OnSessionClose()
}

// NopObserver ignores all events.
type NopObserver struct{}

func (NopObserver) OnRequest(RequestStats)  {}
func (NopObserver) OnConnect(ConnectStats)  {}
func (NopObserver) OnTokenFetch(TokenStats) {}
func (NopObserver) OnSessionOpen()          {}
func (NopObserver) OnSessionClose()         {}

// JSON-RPC wire types.

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
