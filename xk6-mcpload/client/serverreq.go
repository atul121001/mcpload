package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// Responder answers one kind of server-to-client request (sampling,
// elicitation, roots) that a stateful server sends on the SSE response stream
// of a client request. Answers are static: the same Result (or Error) every
// time, after Delay.
type Responder struct {
	// Result is the JSON-RPC result sent back. Empty means {}.
	Result json.RawMessage
	// Error, when set, is sent as a JSON-RPC error instead of Result (e.g.
	// a user rejecting a sampling request).
	Error *ResponderError
	// Delay is waited before answering, to simulate the time an LLM or a
	// human takes.
	Delay time.Duration
}

// ResponderError is a JSON-RPC error a Responder answers with.
type ResponderError struct {
	Code    int
	Message string
}

// ServerRequestStats describes one server-to-client request read from an
// SSE response stream and the client's answer to it.
type ServerRequestStats struct {
	Method    string // the server's JSON-RPC method, e.g. "sampling/createMessage"
	Tool      string // tool name when the request arrived on a tools/call stream
	Protocol  string
	Status    int    // HTTP status of the POST carrying the answer; 0 when none was received
	ErrorType string // "" when answered with a configured result
	Start     time.Time
	Duration  time.Duration // from reading the request off the stream until the answer POST completed (includes Delay)
	// NotAnswered is set when no answer was POSTed: stateless protocol, or
	// the context ended during Delay. Duration is then not meaningful.
	NotAnswered bool
}

// ServerRequestObserver is optionally implemented by an Observer to receive
// server-to-client request events.
type ServerRequestObserver interface {
	OnServerRequest(ServerRequestStats)
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// hasID reports whether a message id is present and not null, i.e. the
// message is a request (with a method) or a response, not a notification.
func hasID(raw json.RawMessage) bool {
	id := bytes.TrimSpace(raw)
	return len(id) > 0 && string(id) != "null"
}

func (s *Session) responder(method string) *Responder {
	switch method {
	case "sampling/createMessage":
		return s.opts.Sampling
	case "elicitation/create":
		return s.opts.Elicitation
	case "roots/list":
		return s.opts.Roots
	}
	return nil
}

// initCaps returns the client capabilities sent by initialize: Options.
// Capabilities plus sampling, elicitation and roots for each configured
// Responder (an explicit entry in Options.Capabilities wins).
func (s *Session) initCaps() map[string]any {
	base := s.clientCaps()
	add := map[string]bool{
		"sampling":    s.opts.Sampling != nil,
		"elicitation": s.opts.Elicitation != nil,
		"roots":       s.opts.Roots != nil,
	}
	out := make(map[string]any, len(base)+3)
	for k, v := range base {
		out[k] = v
	}
	for k, on := range add {
		if _, ok := out[k]; on && !ok {
			out[k] = map[string]any{} // elicitation {} means form mode
		}
	}
	return out
}

// answer handles one server-to-client request that arrived on the stream of
// ex. ctx is the caller's context (not the exchange's timeout context): the
// answer POST has its own Options.Timeout.
//
// Stateful protocols: ping is answered with {}, a method with a configured
// Responder with its result after its Delay, and anything else with -32601
// (counted as unsupported_request). Stateless (2026-07-28): servers MUST NOT
// send requests on response streams and clients MUST NOT POST responses
// (server-to-client interactions use MRTR instead), so nothing is sent and
// the request is counted as unsupported_request.
func (s *Session) answer(ctx context.Context, ex exchange, m *rpcMessage) {
	st := ServerRequestStats{Method: m.Method, Tool: ex.tool, Protocol: s.protocolTag(ex), Start: time.Now()}
	obs, _ := s.obs(ctx).(ServerRequestObserver)
	defer func() {
		st.Duration = time.Since(st.Start)
		if obs != nil {
			obs.OnServerRequest(st)
		}
	}()
	if ex.stateless {
		st.ErrorType, st.NotAnswered = ErrUnsupportedRequest, true
		return
	}
	var result json.RawMessage
	var rerr *rpcError
	r := s.responder(m.Method)
	switch {
	case m.Method == "ping":
		result = json.RawMessage("{}")
	case r == nil:
		st.ErrorType = ErrUnsupportedRequest
		rerr = &rpcError{Code: CodeMethodNotFound, Message: "Method not found: " + m.Method}
	default:
		if r.Delay > 0 {
			t := time.NewTimer(r.Delay)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				st.ErrorType, st.NotAnswered = transportError(ctx, ctx.Err()).Type, true
				return
			}
		}
		if r.Error != nil {
			rerr = &rpcError{Code: r.Error.Code, Message: r.Error.Message}
		} else {
			result = r.Result
		}
	}
	status, err := s.postResponse(ctx, m.ID, result, rerr)
	st.Status = status
	if err != nil && st.ErrorType == "" {
		st.ErrorType = err.Type
	}
}

// postResponse POSTs a JSON-RPC response (2025-xx streamable HTTP: the
// server answers 202 Accepted with no body). It never retries.
func (s *Session) postResponse(ctx context.Context, id json.RawMessage, result json.RawMessage, rerr *rpcError) (int, *Error) {
	msg := rpcResponse{JSONRPC: "2.0", ID: id, Error: rerr}
	if rerr == nil {
		msg.Result = result
		if len(bytes.TrimSpace(msg.Result)) == 0 {
			msg.Result = json.RawMessage("{}")
		}
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return 0, &Error{Type: ErrJSONRPC, Message: "encoding response: " + err.Error()}
	}
	authHdr, aerr := s.authorization(ctx)
	if aerr != nil {
		return 0, aerr
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.opts.URL, bytes.NewReader(body))
	if err != nil {
		return 0, &Error{Type: ErrHTTP, Message: "building request: " + err.Error()}
	}
	s.setCommonHeaders(req, authHdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if s.sessionID != "" {
		req.Header.Set(HeaderSessionID, s.sessionID)
	}
	if s.protocol != "" {
		req.Header.Set(HeaderProtocolVersion, s.protocol)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return 0, transportError(ctx, err)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, s.classifyStatus(resp, b, s.sessionID != "", authHdr)
	}
	return resp.StatusCode, nil
}
