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
	Timeout time.Duration
	// CancelWait is how long the response stream of a cancelled stateful
	// call is still read to detect a late response (0 = DefaultCancelWait).
	CancelWait time.Duration
	HTTPClient *http.Client
	ClientInfo Implementation
	// Capabilities sent as client capabilities. nil means {}.
	Capabilities map[string]any
	// Sampling, Elicitation and Roots answer the server-to-client requests
	// sampling/createMessage, elicitation/create and roots/list that a
	// stateful server sends on a response stream. Each one that is set is
	// also declared in initialize's client capabilities. nil: such requests
	// are answered with -32601 (method not found).
	Sampling    *Responder
	Elicitation *Responder
	Roots       *Responder
	// SkipDiscover disables the server/discover call that Connect makes in
	// explicit stateless mode. (In "auto" mode discover is the probe and is
	// always sent.)
	SkipDiscover bool
	// ProtocolCache, when set and Protocol is "auto", is consulted before
	// probing: a cached resolution skips the server/discover probe (and, for
	// a stateless server, sends nothing on connect). A successful auto
	// negotiation is stored in it; failed connects are never cached.
	ProtocolCache *ProtocolCache
	// ServedByHeader names the response header that identifies the replica
	// that answered (Error.ServedBy, ToolResult.ServedBy, Session.ServedBy).
	// Empty means DefaultServedByHeader.
	ServedByHeader string
	// Observer receives timings; nil means NopObserver.
	Observer Observer
}

// DefaultServedByHeader is the default Options.ServedByHeader.
const DefaultServedByHeader = "X-Served-By"

// Session is a connected MCP session. All methods are safe for concurrent
// use.
type Session struct {
	opts      Options
	hc        *http.Client
	protocol  string
	stateless bool
	sessionID string
	servedBy  string // replica that answered the handshake

	ServerInfo   json.RawMessage
	Capabilities json.RawMessage

	nextID atomic.Int64
	closed atomic.Bool
	// fromCache is set when the session uses a stateless protocol taken from
	// the ProtocolCache without any handshake; the first request that shows
	// the server no longer speaks it drops the cache entry.
	fromCache atomic.Bool
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

// ServedBy returns the ServedByHeader value of the response that completed
// the handshake (server/discover or initialize), or "" (header absent, or a
// cached stateless resolution that sent nothing).
func (s *Session) ServedBy() string { return s.servedBy }

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
	if verr := ValidateProtocol(opts.Protocol); verr != nil {
		return nil, &Error{Type: ErrHTTP, Message: verr.Error()}
	}
	if verr := ValidateFallbackVersion(opts.FallbackVersion); verr != nil {
		return nil, &Error{Type: ErrHTTP, Message: verr.Error()}
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
	if opts.ServedByHeader == "" {
		opts.ServedByHeader = DefaultServedByHeader
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

// connectAuto probes with server/discover (stateless). A modern server that
// answers -32022 (UnsupportedProtocolVersion) listing a newer stateless
// version in error.data.supported is probed once more with that version; the
// stateful handshake is used only when the server doesn't speak any stateless
// protocol revision we can use. If initialize is in turn rejected with -32022
// offering a stateless version (replicas on different builds), that version
// is probed once more.
func (s *Session) connectAuto(ctx context.Context, cs *ConnectStats) (int, *Error) {
	cs.Method = "server/discover"
	obs := s.obs(ctx)
	version := ProtocolStateless
	var status int
	var err *Error
	for attempt := 0; ; attempt++ {
		s.protocol, s.stateless = version, true
		rec := &recordingObserver{next: obs}
		status, err = s.discover(WithObserver(ctx, rec), version)
		retry := ""
		if err != nil && attempt == 0 {
			retry = statelessRetryVersion(err, version)
		}
		fallback := err != nil && retry == "" && shouldFallback(err)
		if rq := rec.recorded(); rq != nil {
			st := *rq
			if retry != "" || fallback {
				// An expected negotiation answer to the probe is not an
				// error; the status tag still shows what the server returned.
				st.ErrorType = ""
			}
			obs.OnRequest(st)
		}
		if retry != "" {
			version = retry
			continue
		}
		if !fallback {
			return status, err
		}
		break
	}
	s.protocol, s.stateless = "", false
	cs.Method = "initialize"
	status, err = s.initialize(ctx, s.opts.FallbackVersion)
	if err != nil {
		// Behind a load balancer in the middle of a rolling deploy, the probe
		// and the handshake can reach different builds: an old one that
		// answers the probe like a legacy server, then a new one that rejects
		// initialize with -32022 and offers a stateless version. Retry once
		// with that version (the rejected initialize stays counted as an
		// error: it is what a client sees during the skew).
		if v := newestStateless(supportedVersions(err), ""); v != "" {
			cs.Method = "server/discover"
			s.protocol, s.stateless = v, true
			return s.discover(ctx, v)
		}
	}
	return status, err
}

// supportedVersions returns error.data.supported of an
// UnsupportedProtocolVersion (-32022) error, or nil.
func supportedVersions(err *Error) []string {
	if err == nil || err.Code != CodeUnsupportedProtocolVersion || len(err.Data) == 0 {
		return nil
	}
	var d struct {
		Supported []string `json:"supported"`
	}
	if json.Unmarshal(err.Data, &d) != nil {
		return nil
	}
	return d.Supported
}

// newestStateless returns the newest stateless (>= 2026-07-28) version in
// vs other than exclude, or "".
func newestStateless(vs []string, exclude string) string {
	best := ""
	for _, v := range vs {
		if v != exclude && IsStateless(v) && ValidProtocolVersion(v) && v > best {
			best = v
		}
	}
	return best
}

// statelessRetryVersion returns the stateless version to retry with after the
// server rejected tried with -32022, or "" when it offers none.
func statelessRetryVersion(err *Error, tried string) string {
	return newestStateless(supportedVersions(err), tried)
}

// connectCached connects with a protocol a previous auto negotiation
// resolved. A stateless resolution needs no handshake, so nothing is sent
// and the cached capabilities/serverInfo are reused; a stateful one runs
// initialize directly, without the server/discover probe.
func (s *Session) connectCached(ctx context.Context, cs *ConnectStats, res Resolution) (int, *Error) {
	if res.Stateless {
		s.protocol, s.stateless = res.Protocol, true
		s.Capabilities, s.ServerInfo = res.Capabilities, res.ServerInfo
		s.fromCache.Store(true)
		return 0, nil
	}
	cs.Method = "initialize"
	return s.initialize(ctx, res.Protocol)
}

// protocolRejected reports whether err means the server refused the
// protocol version itself (as opposed to a transport, auth or server error).
// It uses the same classification as connectAuto: a -32022 is a rejection
// whether or not it offers another stateless version (in which case the
// re-probe on the next connect picks that version), and so is any
// "legacy server" answer (shouldFallback).
func protocolRejected(err *Error) bool {
	return err.Code == CodeUnsupportedProtocolVersion || shouldFallback(err)
}

// shouldFallback implements the spec's rule: fall back to initialize on any
// response that is not a recognised modern (2026-07-28) error. Transport
// failures, timeouts, auth failures and 5xx are not treated as "legacy
// server" signals, and neither is a -32022 whose error.data.supported offers
// a stateless version (that is a modern server: retry with that version).
func shouldFallback(err *Error) bool {
	switch err.Type {
	case ErrTimeout, ErrAuth, ErrHeaderMismatch:
		return false
	}
	if newestStateless(supportedVersions(err), "") != "" {
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
	s.servedBy = r.servedBy
	return r.stats.Status, nil
}

func (s *Session) initialize(ctx context.Context, version string) (int, *Error) {
	id := s.nextID.Add(1)
	s.protocol = version
	r := s.post(ctx, exchange{
		method: "initialize", id: &id,
		params: map[string]any{
			"protocolVersion": version,
			"capabilities":    s.initCaps(),
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
	s.servedBy = r.servedBy
	status := r.stats.Status

	n := s.post(ctx, exchange{
		method: "notifications/initialized", sessionID: s.sessionID, protoHdr: s.protocol,
	})
	if n.err != nil {
		// The server already created a session: release it (best effort)
		// so a failing handshake under load does not leak server sessions.
		if s.sessionID != "" {
			s.abandonSession(ctx)
		}
		return n.stats.Status, n.err
	}
	return status, nil
}

// abandonSession best-effort DELETEs a session that was created by a
// handshake that then failed. It runs even if ctx has ended, bounded by
// Options.Timeout (or abandonTimeout); its outcome is only reported to the
// observer.
func (s *Session) abandonSession(ctx context.Context) {
	dctx := context.WithoutCancel(ctx)
	if s.opts.Timeout <= 0 {
		var cancel context.CancelFunc
		dctx, cancel = context.WithTimeout(dctx, abandonTimeout)
		defer cancel()
	}
	_ = s.deleteSession(dctx)
	s.sessionID = ""
}

// abandonTimeout bounds the cleanup DELETE when Options.Timeout is 0.
const abandonTimeout = 5 * time.Second

// Request sends an arbitrary JSON-RPC request and returns the raw result.
// name is the Mcp-Name header value (stateless) and the tool metric tag for
// tools/call.
func (s *Session) Request(ctx context.Context, method, name string, params map[string]any) (json.RawMessage, *Error) {
	r := s.request(ctx, method, name, params, false, CallOptions{})
	return r.result, r.err
}

func (s *Session) request(ctx context.Context, method, name string, params map[string]any, toolCall bool, co CallOptions) exchangeResult {
	id := s.nextID.Add(1)
	ex := exchange{method: method, id: &id, protoHdr: s.protocol, cancelAfter: co.CancelAfter}
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
	var r exchangeResult
	if toolCall {
		// Classify isError before the observer sees the request.
		r = s.postTool(ctx, ex)
	} else {
		r = s.post(ctx, ex)
	}
	if r.err != nil && staleProtocolError(r.err) && s.fromCache.CompareAndSwap(true, false) {
		// The remembered stateless resolution no longer holds (server
		// downgraded or replaced): make the next connect re-probe.
		s.opts.ProtocolCache.Forget(s.opts.URL, s.opts.Protocol, s.opts.FallbackVersion)
	}
	return r
}

// staleProtocolError reports whether a request error on a session built from
// a cached stateless resolution means the server no longer speaks that
// protocol: a fallback-type answer (shouldFallback) or a -32022, but not an
// ordinary JSON-RPC error returned with HTTP 2xx (e.g. a tool's invalid
// params), which says nothing about the protocol.
func staleProtocolError(err *Error) bool {
	if err.Code == CodeUnsupportedProtocolVersion {
		return true
	}
	if !shouldFallback(err) {
		return false
	}
	return err.HTTPStatus/100 != 2 || err.Code == CodeMethodNotFound
}

// postTool wraps post so that a tools/call result with isError=true is
// reported to the observer with error_type tool_iserror.
func (s *Session) postTool(ctx context.Context, ex exchange) exchangeResult {
	obs := s.obs(ctx)
	rec := &recordingObserver{next: obs}
	r := s.post(WithObserver(ctx, rec), ex)
	if r.err == nil && len(r.result) > 0 {
		var probe struct {
			IsError bool `json:"isError"`
		}
		if json.Unmarshal(r.result, &probe) == nil && probe.IsError {
			r.stats.ErrorType = ErrToolIsError
		}
	}
	if rq := rec.recorded(); rq != nil {
		st := *rq
		st.ErrorType = r.stats.ErrorType
		obs.OnRequest(st)
	}
	return r
}

// recordingObserver holds back the request event of one exchange (so the
// caller can adjust its error type) and forwards token fetches to next right
// away: a background token refresh may report after the exchange is over.
// Only the first request event is held back; later ones (the
// notifications/cancelled of a cancelled call) are forwarded.
type recordingObserver struct {
	NopObserver
	next Observer
	mu   sync.Mutex
	req  *RequestStats
}

func (o *recordingObserver) OnRequest(st RequestStats) {
	o.mu.Lock()
	if o.req == nil {
		o.req = &st
		o.mu.Unlock()
		return
	}
	o.mu.Unlock()
	o.next.OnRequest(st)
}

func (o *recordingObserver) OnTokenFetch(st TokenStats) { o.next.OnTokenFetch(st) }

// OnCancel is forwarded right away: it may come after the call returned.
func (o *recordingObserver) OnCancel(st CancelStats) {
	if n, ok := o.next.(CancelObserver); ok {
		n.OnCancel(st)
	}
}

// OnServerRequest is forwarded right away too: answers are never held back.
func (o *recordingObserver) OnServerRequest(st ServerRequestStats) {
	if n, ok := o.next.(ServerRequestObserver); ok {
		n.OnServerRequest(st)
	}
}

func (o *recordingObserver) recorded() *RequestStats {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.req
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
	return s.CallToolMetaWith(ctx, name, args, nil, CallOptions{})
}

// CallToolMeta is CallTool with params._meta set to meta when it is not
// empty. In stateless mode the protocol's own _meta keys are added to it.
func (s *Session) CallToolMeta(ctx context.Context, name string, args any, meta map[string]any) ToolResult {
	return s.CallToolMetaWith(ctx, name, args, meta, CallOptions{})
}

// CallToolWith is CallTool with per-call options. With CancelAfter > 0 a
// call that has no response within that time returns at once with Cancelled
// set (Err.Type ErrCancelled) and is cancelled on the wire (see cancel.go).
func (s *Session) CallToolWith(ctx context.Context, name string, args any, co CallOptions) ToolResult {
	return s.CallToolMetaWith(ctx, name, args, nil, co)
}

// CallToolMetaWith combines CallToolMeta and CallToolWith: params._meta set
// to meta (when not empty) and the per-call options co.
func (s *Session) CallToolMetaWith(ctx context.Context, name string, args any, meta map[string]any, co CallOptions) ToolResult {
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	} else {
		params["arguments"] = map[string]any{}
	}
	if len(meta) > 0 {
		params["_meta"] = meta
	}
	r := s.request(ctx, "tools/call", name, params, true, co)
	out := ToolResult{Duration: r.stats.Duration, ServedBy: r.servedBy}
	if r.err != nil {
		out.IsError = true
		out.Err = r.err
		out.Cancelled = r.err.Type == ErrCancelled
		return out
	}
	var cr struct {
		Content           json.RawMessage `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(r.result, &cr); err != nil {
		out.IsError = true
		out.Err = &Error{Type: ErrJSONRPC, HTTPStatus: r.stats.Status, Message: "decoding tools/call result: " + err.Error(), ServedBy: r.servedBy}
		return out
	}
	out.Content, out.StructuredContent, out.IsError = cr.Content, cr.StructuredContent, cr.IsError
	if cr.IsError {
		out.Err = &Error{Type: ErrToolIsError, HTTPStatus: r.stats.Status, Message: firstText(cr.Content), ServedBy: r.servedBy}
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
			out[i] = s.CallToolMetaWith(ctx, c.Name, c.Args, c.Meta, c.CallOptions)
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
