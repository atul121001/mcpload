package client

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer emulates an MCP streamable-HTTP server in either stateful
// (2025-xx) or stateless (2026-07-28) mode.
type fakeServer struct {
	t         *testing.T
	stateless bool
	sse       bool // answer requests with text/event-stream
	token     string
	slow      time.Duration

	mu       sync.Mutex
	sessions map[string]bool
	nextSess int
	deleted  []string
	lastHdr  http.Header
	lastBody map[string]any
	reqs     atomic.Int64
}

func newFake(t *testing.T, stateless bool) (*fakeServer, *httptest.Server) {
	f := &fakeServer{t: t, stateless: stateless, sessions: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func rpcErr(id any, code int, msg string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}}
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.reqs.Add(1)
	if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodDelete {
		if f.stateless {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.mu.Lock()
		sid := r.Header.Get(HeaderSessionID)
		delete(f.sessions, sid)
		f.deleted = append(f.deleted, sid)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}
	b, _ := io.ReadAll(r.Body)
	var msg struct {
		ID     any            `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(b, &msg); err != nil {
		writeJSON(w, 400, rpcErr(nil, -32700, "parse error"))
		return
	}
	f.mu.Lock()
	f.lastHdr = r.Header.Clone()
	f.lastBody = msg.Params
	f.mu.Unlock()

	if f.stateless {
		f.serveStateless(w, r, msg.ID, msg.Method, msg.Params)
		return
	}
	f.serveStateful(w, r, msg.ID, msg.Method, msg.Params)
}

func (f *fakeServer) serveStateful(w http.ResponseWriter, r *http.Request, id any, method string, params map[string]any) {
	sid := r.Header.Get(HeaderSessionID)
	if method == "initialize" {
		f.mu.Lock()
		f.nextSess++
		sid = fmt.Sprintf("sess-%d", f.nextSess)
		f.sessions[sid] = true
		f.mu.Unlock()
		w.Header().Set(HeaderSessionID, sid)
		f.reply(w, id, map[string]any{
			"protocolVersion": params["protocolVersion"],
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fake", "version": "1"},
		})
		return
	}
	if sid == "" {
		writeJSON(w, 400, rpcErr(nil, -32000, "Bad Request: no Mcp-Session-Id header and body is not an initialize request"))
		return
	}
	f.mu.Lock()
	ok := f.sessions[sid]
	f.mu.Unlock()
	if !ok {
		writeJSON(w, 404, rpcErr(nil, -32001, "Session not found"))
		return
	}
	if r.Header.Get(HeaderProtocolVersion) == "" {
		writeJSON(w, 400, rpcErr(id, -32000, "missing protocol version"))
		return
	}
	if method == "notifications/initialized" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	f.handle(w, id, method, params)
}

func (f *fakeServer) serveStateless(w http.ResponseWriter, r *http.Request, id any, method string, params map[string]any) {
	meta, _ := params["_meta"].(map[string]any)
	if meta == nil || meta[MetaProtocolVersion] != ProtocolStateless || meta[MetaClientInfo] == nil || meta[MetaClientCapabilities] == nil {
		writeJSON(w, 400, rpcErr(id, -32600, "missing _meta"))
		return
	}
	if r.Header.Get(HeaderMethod) != method || r.Header.Get(HeaderProtocolVersion) != ProtocolStateless {
		writeJSON(w, 400, rpcErr(id, CodeHeaderMismatch, "header mismatch"))
		return
	}
	if method == "tools/call" {
		name := r.Header.Get(HeaderName)
		if strings.HasPrefix(name, "=?base64?") {
			dec, _ := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(name, "=?base64?"), "?="))
			name = string(dec)
		}
		if name != params["name"] {
			writeJSON(w, 400, rpcErr(id, CodeHeaderMismatch, "Mcp-Name mismatch"))
			return
		}
	}
	if method == "server/discover" {
		f.reply(w, id, map[string]any{
			"supportedVersions": []string{"2026-07-28", "2025-11-25"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
		})
		return
	}
	f.handle(w, id, method, params)
}

func (f *fakeServer) handle(w http.ResponseWriter, id any, method string, params map[string]any) {
	switch method {
	case "ping":
		f.reply(w, id, map[string]any{})
	case "tools/list":
		cursor, _ := params["cursor"].(string)
		if cursor == "" {
			f.reply(w, id, map[string]any{
				"tools":      []any{map[string]any{"name": "echo", "description": "echoes", "inputSchema": map[string]any{"type": "object"}}},
				"nextCursor": "p2",
			})
			return
		}
		f.reply(w, id, map[string]any{"tools": []any{map[string]any{"name": "fail"}, map[string]any{"name": "slow"}}})
	case "tools/call":
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		switch name {
		case "fail":
			f.reply(w, id, map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "boom"}}})
		case "slow":
			time.Sleep(f.slow)
			f.reply(w, id, map[string]any{"content": []any{}})
		case "rpcerror":
			f.reply(w, id, nil) // replaced below
		default:
			f.reply(w, id, map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprint(args["msg"])}}})
		}
	default:
		writeJSON(w, 200, rpcErr(id, -32601, "method not found"))
	}
}

func (f *fakeServer) reply(w http.ResponseWriter, id any, result any) {
	if result == nil {
		writeJSON(w, 200, rpcErr(id, -32603, "internal"))
		return
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
	if !f.sse {
		writeJSON(w, 200, resp)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	// A notification and a keep-alive comment first, then the response.
	fmt.Fprint(w, ": keep-alive\n\n")
	fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
	if fl != nil {
		fl.Flush()
	}
	time.Sleep(5 * time.Millisecond)
	b, _ := json.Marshal(resp)
	fmt.Fprintf(w, "event: message\nid: 1\ndata: %s\n\n", b)
}

// collector is a thread-safe Observer for assertions.
type collector struct {
	mu       sync.Mutex
	reqs     []RequestStats
	connects []ConnectStats
	tokens   []TokenStats
	open     int
}

func (c *collector) OnRequest(s RequestStats) { c.mu.Lock(); c.reqs = append(c.reqs, s); c.mu.Unlock() }
func (c *collector) OnConnect(s ConnectStats) {
	c.mu.Lock()
	c.connects = append(c.connects, s)
	c.mu.Unlock()
}
func (c *collector) OnTokenFetch(s TokenStats) {
	c.mu.Lock()
	c.tokens = append(c.tokens, s)
	c.mu.Unlock()
}
func (c *collector) OnSessionOpen()  { c.mu.Lock(); c.open++; c.mu.Unlock() }
func (c *collector) OnSessionClose() { c.mu.Lock(); c.open--; c.mu.Unlock() }

func (c *collector) byMethod(m string) []RequestStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []RequestStats
	for _, r := range c.reqs {
		if r.Method == m {
			out = append(out, r)
		}
	}
	return out
}
