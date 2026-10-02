package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// --wait-ready: before k6 starts, poll the MCP endpoint until it answers a
// real handshake. The probe mirrors xk6-mcpload's connect: server/discover
// for a stateless protocol, initialize for a stateful one, and for "auto"
// server/discover first with the same fallback to initialize.

const (
	// readyStateless and readyFallback mirror the extension's
	// ProtocolStateless and DefaultFallbackVersion.
	readyStateless = "2026-07-28"
	readyFallback  = "2025-11-25"
	// codeUnsupportedProtocol is the 2026-07-28 UnsupportedProtocolVersion error.
	codeUnsupportedProtocol = -32022
	readyMaxBody            = 1 << 20
)

// readyProbe is one readiness check against an MCP endpoint.
type readyProbe struct {
	URL      string
	Protocol string            // "auto" or a protocol version
	Headers  map[string]string // MCP_HEADERS plus Authorization for MCP_TOKEN
	// OAuth is set when the test authenticates with OAuth client credentials.
	// The probe does not fetch a token, so a 401/403 answer means the server
	// is up (it is answering, just not to anonymous requests).
	OAuth  bool
	Client *http.Client
	// Timeout bounds one probe attempt (default 5s).
	Timeout time.Duration
}

// readyWait controls the polling loop; zero fields take the defaults.
type readyWait struct {
	Max      time.Duration // give up after this long
	Interval time.Duration // first retry delay (default 250ms), doubled per attempt
	MaxDelay time.Duration // retry delay cap (default 2s)
	LogEvery time.Duration // progress log interval (default 5s)
}

// errNotReady is a probe outcome meaning "try again".
type errNotReady struct{ reason string }

func (e *errNotReady) Error() string { return e.reason }

func notReady(format string, a ...any) error { return &errNotReady{fmt.Sprintf(format, a...)} }

// newReadyProbe builds the probe from the env the k6 script sees, so it uses
// the same URL, protocol, headers and bearer token as the test.
func newReadyProbe(env map[string]string) (*readyProbe, error) {
	p := &readyProbe{URL: env["MCP_URL"], Protocol: env["MCP_PROTOCOL"], Headers: map[string]string{}}
	if p.Protocol == "" {
		p.Protocol = "auto"
	}
	if h := env["MCP_HEADERS"]; h != "" {
		var m map[string]any
		if err := json.Unmarshal([]byte(h), &m); err != nil || m == nil {
			return nil, errors.New("MCP_HEADERS must be a JSON object")
		}
		for k, v := range m {
			if s, ok := v.(string); ok {
				p.Headers[k] = s
			} else {
				p.Headers[k] = fmt.Sprint(v)
			}
		}
	}
	// Same precedence as scenarios/lib/config.js: OAuth wins over MCP_TOKEN.
	if env["OAUTH_TOKEN_URL"] != "" {
		p.OAuth = true
	} else if tok := env["MCP_TOKEN"]; tok != "" {
		p.Headers["Authorization"] = "Bearer " + tok
	}
	return p, nil
}

// waitReady polls p until it succeeds, w.Max elapses (error naming the last
// failure), the server rejects the credentials (401/403 without OAuth), or
// ctx is cancelled.
func waitReady(ctx context.Context, p *readyProbe, w readyWait, logf func(string, ...any)) error {
	if w.Interval <= 0 {
		w.Interval = 250 * time.Millisecond
	}
	if w.MaxDelay <= 0 {
		w.MaxDelay = 2 * time.Second
	}
	if w.LogEvery <= 0 {
		w.LogEvery = 5 * time.Second
	}
	start := time.Now()
	deadline := start.Add(w.Max)
	dctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	lastLog := start
	delay := w.Interval
	last := ""
	for {
		err := p.check(dctx)
		var nr *errNotReady
		switch {
		case err == nil:
			logf("%s ready after %.1fs", p.URL, time.Since(start).Seconds())
			return nil
		case ctx.Err() != nil:
			return fmt.Errorf("interrupted while waiting for %s to become ready", p.URL)
		case !errors.As(err, &nr):
			return err
		}
		// An attempt cut short by the overall deadline says nothing new
		// ("timeout"); report the previous failure instead.
		if dctx.Err() == nil || last == "" {
			last = nr.reason
		}
		now := time.Now()
		if !now.Before(deadline) {
			return fmt.Errorf("%s not ready after %s (last: %s)", p.URL, w.Max, last)
		}
		if now.Sub(lastLog) >= w.LogEvery {
			logf("waiting for %s to become ready (%s, last: %s)", p.URL, now.Sub(start).Round(time.Second), last)
			lastLog = now
		}
		t := time.NewTimer(min(delay, deadline.Sub(now)))
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("interrupted while waiting for %s to become ready", p.URL)
		case <-t.C:
		}
		delay = min(delay*2, w.MaxDelay)
	}
}

// check runs one handshake: nil = ready, *errNotReady = retry, anything else
// stops the wait.
func (p *readyProbe) check(ctx context.Context) error {
	switch {
	case p.Protocol == "auto":
		version := readyStateless
		for attempt := 0; ; attempt++ {
			r := p.post(ctx, version, "server/discover", true)
			if r.err == nil {
				return nil
			}
			if r.final || !r.answered {
				return r.err
			}
			// A modern server that rejects 2026-07-28 but lists a newer
			// stateless version is probed once more with it.
			if v := r.newerStateless(version); v != "" && attempt == 0 {
				version = v
				continue
			}
			if r.status >= 500 {
				return r.err
			}
			break
		}
		return p.post(ctx, readyFallback, "initialize", false).err
	case p.Protocol >= readyStateless:
		return p.post(ctx, p.Protocol, "server/discover", true).err
	default:
		return p.post(ctx, p.Protocol, "initialize", false).err
	}
}

type probeResult struct {
	err      error
	final    bool // stop polling (auth failure)
	answered bool // the server sent an HTTP response
	status   int
	rpcErr   *rpcErr
}

type rpcErr struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type rpcMsg struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcErr         `json:"error"`
}

// newerStateless returns the newest stateless version other than tried that
// a -32022 error lists in data.supported, or "".
func (r probeResult) newerStateless(tried string) string {
	if r.rpcErr == nil || r.rpcErr.Code != codeUnsupportedProtocol {
		return ""
	}
	var d struct {
		Supported []string `json:"supported"`
	}
	_ = json.Unmarshal(r.rpcErr.Data, &d)
	best := ""
	for _, v := range d.Supported {
		if v != tried && v >= readyStateless && len(v) == len(readyStateless) && v > best {
			best = v
		}
	}
	return best
}

func (p *readyProbe) post(ctx context.Context, version, method string, stateless bool) probeResult {
	var params map[string]any
	info := map[string]string{"name": "mcpload", "version": Version}
	if stateless {
		params = map[string]any{"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    version,
			"io.modelcontextprotocol/clientInfo":         info,
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		}}
	} else {
		params = map[string]any{"protocolVersion": version, "capabilities": map[string]any{}, "clientInfo": info}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})

	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewReader(body))
	if err != nil {
		return probeResult{err: err, final: true}
	}
	p.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if stateless {
		req.Header.Set("Mcp-Protocol-Version", version)
		req.Header.Set("Mcp-Method", method)
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return probeResult{err: notReady("%s", transportReason(ctx, err))}
	}
	defer resp.Body.Close()
	r := probeResult{answered: true, status: resp.StatusCode}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		defer p.deleteSession(sid, version)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		if p.OAuth {
			// Answering an anonymous request with 401/403 means the server is
			// up; the test fetches the OAuth token itself.
			return r
		}
		r.err = fmt.Errorf("%s answered HTTP %d to the readiness probe: the server rejected the credentials (check MCP_TOKEN / MCP_HEADERS); not waiting any longer", p.URL, resp.StatusCode)
		r.final = true
		return r
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, readyMaxBody))
	if resp.StatusCode/100 != 2 {
		r.rpcErr = findRPC(resp.Header.Get("Content-Type"), b).Error
		r.err = notReady("HTTP %d%s", resp.StatusCode, rpcSuffix(r.rpcErr))
		return r
	}
	if err != nil && len(b) == 0 {
		r.err = notReady("reading %s response: %s", method, transportReason(ctx, err))
		return r
	}
	msg := findRPC(resp.Header.Get("Content-Type"), b)
	switch {
	case msg.Error != nil:
		r.rpcErr = msg.Error
		r.err = notReady("%s returned JSON-RPC error %d: %s", method, msg.Error.Code, msg.Error.Message)
	case len(msg.Result) == 0 || string(msg.Result) == "null":
		r.err = notReady("%s answered HTTP %d without a JSON-RPC result", method, resp.StatusCode)
	}
	return r
}

func rpcSuffix(e *rpcErr) string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf(" (JSON-RPC error %d: %s)", e.Code, e.Message)
}

func (p *readyProbe) setHeaders(req *http.Request) {
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "mcpload/"+Version)
	}
}

func (p *readyProbe) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

// deleteSession ends the stateful session the probe opened (best effort).
func (p *readyProbe) deleteSession(sid, version string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, p.URL, nil)
	if err != nil {
		return
	}
	p.setHeaders(req)
	req.Header.Set("Mcp-Session-Id", sid)
	req.Header.Set("Mcp-Protocol-Version", version)
	if resp, err := p.client().Do(req); err == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, readyMaxBody))
		_ = resp.Body.Close()
	}
}

// findRPC extracts the first JSON-RPC response (a message with an id and a
// result or error) from a JSON body or an SSE stream's data: lines.
func findRPC(contentType string, b []byte) rpcMsg {
	mt, _, _ := mime.ParseMediaType(contentType)
	if mt != "text/event-stream" {
		return pickRPC(b)
	}
	var data []string
	flush := func() rpcMsg {
		m := pickRPC([]byte(strings.Join(data, "\n")))
		data = data[:0]
		return m
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), readyMaxBody)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if m := flush(); m.Error != nil || len(m.Result) > 0 {
				return m
			}
			continue
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(v, " "))
		}
	}
	return flush()
}

func pickRPC(b []byte) rpcMsg {
	b = bytes.TrimSpace(b)
	var msgs []rpcMsg
	if len(b) > 0 && b[0] == '[' {
		_ = json.Unmarshal(b, &msgs)
	} else {
		var m rpcMsg
		if json.Unmarshal(b, &m) == nil {
			msgs = append(msgs, m)
		}
	}
	for _, m := range msgs {
		if len(m.ID) > 0 && string(m.ID) != "null" && (m.Error != nil || len(m.Result) > 0) {
			return m
		}
	}
	return rpcMsg{}
}

// transportReason is a short description of a failed request.
func transportReason(ctx context.Context, err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED), strings.Contains(err.Error(), "refused"):
		return "connection refused"
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return err.Error()
}
