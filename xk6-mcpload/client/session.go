package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Version is reported in clientInfo.
const Version = "0.1.0"

// Options configures a Session.
type Options struct {
	URL string
	// Protocol is "auto", "2026-07-28" (stateless) or a stateful version such
	// as "2025-06-18". Empty means "auto".
	Protocol string
	// FallbackVersion is requested by initialize when "auto" falls back to
	// the stateful handshake. Empty means DefaultFallbackVersion.
	FallbackVersion string
	Headers         map[string]string
	Auth            Auth
	// Timeout bounds every HTTP exchange (0 = no timeout).
	Timeout    time.Duration
	HTTPClient *http.Client
	ClientInfo Implementation
	// Capabilities sent as client capabilities. nil means {}.
	Capabilities map[string]any
	// SkipDiscover disables the server/discover call that Connect makes in
	// explicit stateless mode. (In "auto" mode discover is the probe and is
	// always sent.)
	SkipDiscover bool
	// ProtocolCache, when set and Protocol is "auto", is consulted before
	// probing: a cached resolution skips the server/discover probe (and, for
	// a stateless server, sends nothing on connect). A successful auto
	// negotiation is stored in it; failed connects are never cached.
	ProtocolCache *ProtocolCache
	// Observer receives timings; nil means NopObserver.
	Observer Observer
}

// Session is a connected MCP session. All methods are safe for concurrent
// use.
type Session struct {
	opts      Options
	hc        *http.Client
	protocol  string
	stateless bool
	sessionID string

	ServerInfo   json.RawMessage
	Capabilities json.RawMessage

	nextID atomic.Int64
	closed atomic.Bool
}

type observerKey struct{}

// WithObserver returns a context whose operations report to obs instead of
// the session's default observer.
func WithObserver(ctx context.Context, obs Observer) context.Context {
	return context.WithValue(ctx, observerKey{}, obs)
}

func (s *Session) obs(ctx context.Context) Observer {
	if o, ok := ctx.Value(observerKey{}).(Observer); ok && o != nil {
		return o
	}
	return s.opts.Observer
}

// Protocol returns the negotiated protocol version.
func (s *Session) Protocol() string { return s.protocol }

// SessionID returns the Mcp-Session-Id (stateful mode), or "".
func (s *Session) SessionID() string { return s.sessionID }

// Stateless reports whether the session uses the stateless protocol.
func (s *Session) Stateless() bool { return s.stateless }

func (s *Session) protocolTag(ex exchange) string {
	if s.protocol != "" {
		return s.protocol
	}
	if ex.stateless {
		return ProtocolStateless
	}
	return ""
}

func (s *Session) clientCaps() map[string]any {
	if s.opts.Capabilities != nil {
		return s.opts.Capabilities
	}
	return map[string]any{}
}

// Connect performs the handshake for the configured protocol and returns a
// session. A failed connect returns *Error.
func Connect(ctx context.Context, opts Options) (*Session, error) {
	if opts.Protocol == "" {
		opts.Protocol = ProtocolAuto
	}
	if opts.FallbackVersion == "" {
		opts.FallbackVersion = DefaultFallbackVersion
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	if opts.Observer == nil {
		opts.Observer = NopObserver{}
	}
	if opts.ClientInfo.Name == "" {
		opts.ClientInfo = Implementation{Name: "mcpload", Version: Version}
	}
	s := &Session{opts: opts, hc: opts.HTTPClient}
	obs := s.obs(ctx)
	start := time.Now()
	cs := ConnectStats{Start: start}

	var err *Error
	var status int
	switch {
	case opts.Protocol == ProtocolAuto && opts.ProtocolCache != nil:
		pc := opts.ProtocolCache
		if res, ok := pc.Load(opts.URL, opts.Protocol, opts.FallbackVersion); ok {
			status, err = s.connectCached(ctx, &cs, res)
			if err != nil && protocolRejected(err) {
				// The server no longer accepts the remembered protocol:
				// re-probe on the next connect.
				pc.Forget(opts.URL, opts.Protocol, opts.FallbackVersion)
			}
		} else {
			status, err = s.connectAuto(ctx, &cs)
			if err == nil {
				pc.Store(opts.URL, opts.Protocol, opts.FallbackVersion, Resolution{
					Protocol: s.protocol, Stateless: s.stateless,
					Capabilities: s.Capabilities, ServerInfo: s.ServerInfo,
				})
			}
		}
	case opts.Protocol == ProtocolAuto:
		status, err = s.connectAuto(ctx, &cs)
	case IsStateless(opts.Protocol):
		s.protocol, s.stateless = opts.Protocol, true
		if !opts.SkipDiscover {
			cs.Method = "server/discover"
			status, err = s.discover(ctx, opts.Protocol)
		}
	default:
		cs.Method = "initialize"
		status, err = s.initialize(ctx, opts.Protocol)
	}
	cs.Protocol = s.protocol
	cs.Status = status
	cs.Duration = time.Since(start)
	if err != nil {
		cs.ErrorType = err.Type
		obs.OnConnect(cs)
		return nil, err
	}
	obs.OnConnect(cs)
	obs.OnSessionOpen()
	return s, nil
}

// connectAuto probes with server/discover (stateless) and falls back to the
// stateful handshake when the server doesn't speak the modern protocol.
func (s *Session) connectAuto(ctx context.Context, cs *ConnectStats) (int, *Error) {
	cs.Method = "server/discover"
	s.protocol, s.stateless = ProtocolStateless, true
	obs := s.obs(ctx)
	rec := &recordingObserver{}
	status, err := s.discover(WithObserver(ctx, rec), ProtocolStateless)
	fallback := err != nil && shouldFallback(err)
	for _, t := range rec.tokens {
		obs.OnTokenFetch(t)
	}
	if rec.req != nil {
		st := *rec.req
		if fallback {
			// An expected "legacy server" answer to the probe is not an error;
			// the status tag still shows what the server returned.
			st.ErrorType = ""
		}
		obs.OnRequest(st)
	}
	if !fallback {
		return status, err
	}
	s.protocol, s.stateless = "", false
	cs.Method = "initialize"
	return s.initialize(ctx, s.opts.FallbackVersion)
}

// connectCached connects with a protocol a previous auto negotiation
// resolved. A stateless resolution needs no handshake, so nothing is sent
// and the cached capabilities/serverInfo are reused; a stateful one runs
// initialize directly, without the server/discover probe.
func (s *Session) connectCached(ctx context.Context, cs *ConnectStats, res Resolution) (int, *Error) {
	if res.Stateless {
		s.protocol, s.stateless = res.Protocol, true
		s.Capabilities, s.ServerInfo = res.Capabilities, res.ServerInfo
		return 0, nil
	}
	cs.Method = "initialize"
	return s.initialize(ctx, res.Protocol)
}

// protocolRejected reports whether err means the server refused the
// protocol version itself (as opposed to a transport, auth or server error).
func protocolRejected(err *Error) bool {
	return err.Code == CodeUnsupportedProtocolVersion || shouldFallback(err)
}

// shouldFallback implements the spec's rule: fall back to initialize on any
// response that is not a recognised modern (2026-07-28) error. Transport
// failures, timeouts, auth failures and 5xx are not treated as "legacy
// server" signals.
func shouldFallback(err *Error) bool {
	switch err.Type {
	case ErrTimeout, ErrAuth, ErrHeaderMismatch:
		return false
	}
	if err.HTTPStatus == 0 && err.Code == 0 {
		return false // transport error
	}
	if err.HTTPStatus >= 500 {
		return false
	}
	switch err.Code {
	case CodeHeaderMismatch, CodeMissingRequiredClientCapabilities:
		return false
	}
	// Includes CodeUnsupportedProtocolVersion (no common modern version),
	// HTTP 400/404/405 with a non-modern body, and -32601 method not found.
	return true
}

func (s *Session) statelessParams(version string, params map[string]any) map[string]any {
	out := make(map[string]any, len(params)+1)
	for k, v := range params {
		out[k] = v
	}
	meta := map[string]any{}
	if m, ok := params["_meta"].(map[string]any); ok {
		for k, v := range m {
			meta[k] = v
		}
	}
	meta[MetaProtocolVersion] = version
	meta[MetaClientInfo] = s.opts.ClientInfo
	meta[MetaClientCapabilities] = s.clientCaps()
	out["_meta"] = meta
	return out
}

func (s *Session) discover(ctx context.Context, version string) (int, *Error) {
	id := s.nextID.Add(1)
	r := s.post(ctx, exchange{
		method: "server/discover", id: &id, stateless: true, protoHdr: version,
		params: s.statelessParams(version, nil),
	})
	if r.err != nil {
		return r.stats.Status, r.err
	}
	var dr struct {
		SupportedVersions []string                   `json:"supportedVersions"`
		Capabilities      json.RawMessage            `json:"capabilities"`
		Meta              map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(r.result, &dr); err != nil {
		return r.stats.Status, &Error{Type: ErrJSONRPC, HTTPStatus: r.stats.Status, Message: "decoding server/discover result: " + err.Error()}
	}
	if len(dr.SupportedVersions) > 0 && !slices.Contains(dr.SupportedVersions, version) {
		// Pick the newest mutually usable stateless version, if any.
		best := ""
		for _, v := range dr.SupportedVersions {
			if IsStateless(v) && v > best {
				best = v
			}
		}
		if best == "" {
			return r.stats.Status, &Error{Type: ErrJSONRPC, HTTPStatus: r.stats.Status, Code: CodeUnsupportedProtocolVersion,
				Message: "server/discover lists no stateless protocol version"}
		}
		version = best
	}
	s.protocol = version
	s.Capabilities = dr.Capabilities
	s.ServerInfo = dr.Meta["io.modelcontextprotocol/serverInfo"]
	return r.stats.Status, nil
}

func (s *Session) initialize(ctx context.Context, version string) (int, *Error) {
	id := s.nextID.Add(1)
	s.protocol = version
	r := s.post(ctx, exchange{
		method: "initialize", id: &id,
		params: map[string]any{
			"protocolVersion": version,
			"capabilities":    s.clientCaps(),
			"clientInfo":      s.opts.ClientInfo,
		},
	})
	if r.err != nil {
		return r.stats.Status, r.err
	}
	var ir struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
		ServerInfo      json.RawMessage `json:"serverInfo"`
	}
	if err := json.Unmarshal(r.result, &ir); err != nil {
		return r.stats.Status, &Error{Type: ErrJSONRPC, HTTPStatus: r.stats.Status, Message: "decoding initialize result: " + err.Error()}
	}
	if ir.ProtocolVersion != "" {
		s.protocol = ir.ProtocolVersion
	}
	s.sessionID = r.sessionID
	s.Capabilities, s.ServerInfo = ir.Capabilities, ir.ServerInfo
	status := r.stats.Status

	n := s.post(ctx, exchange{
		method: "notifications/initialized", sessionID: s.sessionID, protoHdr: s.protocol,
	})
	if n.err != nil {
		return n.stats.Status, n.err
	}
	return status, nil
}

// Request sends an arbitrary JSON-RPC request and returns the raw result.
// name is the Mcp-Name header value (stateless) and the tool metric tag for
// tools/call.
func (s *Session) Request(ctx context.Context, method, name string, params map[string]any) (json.RawMessage, *Error) {
	r := s.request(ctx, method, name, params, false)
	return r.result, r.err
}

func (s *Session) request(ctx context.Context, method, name string, params map[string]any, toolCall bool) exchangeResult {
	id := s.nextID.Add(1)
	ex := exchange{method: method, id: &id, protoHdr: s.protocol}
	if toolCall {
		ex.tool = name
	}
	if s.stateless {
		ex.stateless = true
		ex.nameHdr = name
		ex.params = s.statelessParams(s.protocol, params)
	} else {
		ex.sessionID = s.sessionID
		if params != nil {
			ex.params = params
		}
	}
	if toolCall {
		// Classify isError before the observer sees the request.
		return s.postTool(ctx, ex)
	}
	return s.post(ctx, ex)
}

// postTool wraps post so that a tools/call result with isError=true is
// reported to the observer with error_type tool_iserror.
func (s *Session) postTool(ctx context.Context, ex exchange) exchangeResult {
	rec := &recordingObserver{}
	r := s.post(WithObserver(ctx, rec), ex)
	obs := s.obs(ctx)
	for _, t := range rec.tokens {
		obs.OnTokenFetch(t)
	}
	if r.err == nil && len(r.result) > 0 {
		var probe struct {
			IsError bool `json:"isError"`
		}
		if json.Unmarshal(r.result, &probe) == nil && probe.IsError {
			r.stats.ErrorType = ErrToolIsError
		}
	}
	if rec.req != nil {
		st := *rec.req
		st.ErrorType = r.stats.ErrorType
		obs.OnRequest(st)
	}
	return r
}

type recordingObserver struct {
	NopObserver
	mu     sync.Mutex
	req    *RequestStats
	tokens []TokenStats
}

func (o *recordingObserver) OnRequest(st RequestStats) {
	o.mu.Lock()
	o.req = &st
	o.mu.Unlock()
}

func (o *recordingObserver) OnTokenFetch(st TokenStats) {
	o.mu.Lock()
	o.tokens = append(o.tokens, st)
	o.mu.Unlock()
}

// ListTools returns all tools, following nextCursor pagination.
func (s *Session) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 10000; page++ {
		var params map[string]any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		raw, err := s.Request(ctx, "tools/list", "", params)
		if err != nil {
			return all, err
		}
		var lr struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if uerr := json.Unmarshal(raw, &lr); uerr != nil {
			return all, &Error{Type: ErrJSONRPC, Message: "decoding tools/list result: " + uerr.Error()}
		}
		all = append(all, lr.Tools...)
		if lr.NextCursor == "" || seen[lr.NextCursor] {
			return all, nil
		}
		seen[lr.NextCursor] = true
		cursor = lr.NextCursor
	}
	return all, nil
}

// CallTool invokes tools/call. MCP-level failures are reported in the
// result's Err, never as a Go error.
func (s *Session) CallTool(ctx context.Context, name string, args any) ToolResult {
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	} else {
		params["arguments"] = map[string]any{}
	}
	r := s.request(ctx, "tools/call", name, params, true)
	out := ToolResult{Duration: r.stats.Duration}
	if r.err != nil {
		out.IsError = true
		out.Err = r.err
		return out
	}
	var cr struct {
		Content           json.RawMessage `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(r.result, &cr); err != nil {
		out.IsError = true
		out.Err = &Error{Type: ErrJSONRPC, HTTPStatus: r.stats.Status, Message: "decoding tools/call result: " + err.Error()}
		return out
	}
	out.Content, out.StructuredContent, out.IsError = cr.Content, cr.StructuredContent, cr.IsError
	if cr.IsError {
		out.Err = &Error{Type: ErrToolIsError, HTTPStatus: r.stats.Status, Message: firstText(cr.Content)}
	}
	return out
}

func firstText(content json.RawMessage) string {
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &items) == nil {
		for _, it := range items {
			if it.Type == "text" {
				return it.Text
			}
		}
	}
	return "tool returned isError=true"
}

// CallParallel runs the calls concurrently and returns results in input
// order.
func (s *Session) CallParallel(ctx context.Context, calls []ToolCall) []ToolResult {
	out := make([]ToolResult, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func(i int, c ToolCall) {
			defer wg.Done()
			out[i] = s.CallTool(ctx, c.Name, c.Args)
		}(i, c)
	}
	wg.Wait()
	return out
}

// Ping checks liveness. The 2026-07-28 protocol removed "ping", so in
// stateless mode a server/discover request is sent instead.
func (s *Session) Ping(ctx context.Context) error {
	method := "ping"
	if s.stateless {
		method = "server/discover"
	}
	if _, err := s.Request(ctx, method, "", nil); err != nil {
		return err
	}
	return nil
}

// Close ends the session: DELETE with the session id in stateful mode, a
// no-op on the wire in stateless mode. Close is idempotent.
func (s *Session) Close(ctx context.Context) error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	obs := s.obs(ctx)
	defer obs.OnSessionClose()
	if s.stateless || s.sessionID == "" {
		return nil
	}
	return s.deleteSession(ctx)
}

func (s *Session) deleteSession(ctx context.Context) error {
	st := RequestStats{Method: "DELETE", Protocol: s.protocol, Start: time.Now()}
	obs := s.obs(ctx)
	authHdr, aerr := s.authorization(ctx)
	if aerr != nil {
		st.ErrorType = aerr.Type
		st.NotSent = true
		obs.OnRequest(st)
		return aerr
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.opts.URL, nil)
	if err != nil {
		return &Error{Type: ErrHTTP, Message: err.Error()}
	}
	s.setCommonHeaders(req, authHdr)
	req.Header.Set(HeaderSessionID, s.sessionID)
	req.Header.Set(HeaderProtocolVersion, s.protocol)
	start := time.Now()
	st.Start = start
	resp, err := s.hc.Do(req)
	if err != nil {
		e := transportError(ctx, err)
		st.Duration, st.ErrorType = time.Since(start), e.Type
		obs.OnRequest(st)
		return e
	}
	st.TTFB = time.Since(start)
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
	st.Status = resp.StatusCode
	st.Duration = time.Since(start)
	var e *Error
	// 405: server does not allow client-initiated termination; not an error.
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusMethodNotAllowed {
		e = s.classifyStatus(resp, body, true, authHdr)
		st.ErrorType = e.Type
	}
	obs.OnRequest(st)
	if e != nil {
		return e
	}
	return nil
}

// AsError converts err to *Error when possible.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Type: ErrHTTP, Message: err.Error()}
}
