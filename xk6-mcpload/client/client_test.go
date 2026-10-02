package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func connect(t *testing.T, url, protocol string, obs Observer, mod ...func(*Options)) *Session {
	t.Helper()
	o := Options{URL: url, Protocol: protocol, Observer: obs, Timeout: 5 * time.Second}
	for _, m := range mod {
		m(&o)
	}
	s, err := Connect(context.Background(), o)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return s
}

func TestStatefulLifecycle(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[sse], func(t *testing.T) {
			f, srv := newFake(t, false)
			f.sse = sse
			obs := &collector{}
			s := connect(t, srv.URL, "2025-06-18", obs)
			if s.Protocol() != "2025-06-18" || s.SessionID() != "sess-1" || s.Stateless() {
				t.Fatalf("bad session: %q %q", s.Protocol(), s.SessionID())
			}
			if len(obs.byMethod("notifications/initialized")) != 1 {
				t.Fatal("notifications/initialized not sent")
			}
			tools, err := s.ListTools(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(tools) != 3 || tools[0].Name != "echo" || string(tools[0].InputSchema) != `{"type":"object"}` {
				t.Fatalf("tools: %+v", tools)
			}
			if n := len(obs.byMethod("tools/list")); n != 2 {
				t.Fatalf("expected 2 tools/list pages, got %d", n)
			}
			f.mu.Lock()
			if f.lastHdr.Get(HeaderSessionID) != "sess-1" || f.lastHdr.Get(HeaderProtocolVersion) != "2025-06-18" {
				t.Errorf("headers: %v", f.lastHdr)
			}
			if f.lastHdr.Get(HeaderMethod) != "" {
				t.Error("stateful mode must not send Mcp-Method")
			}
			f.mu.Unlock()

			r := s.CallTool(context.Background(), "echo", map[string]any{"msg": "hi"})
			if r.Err != nil || r.IsError || !strings.Contains(string(r.Content), `"hi"`) {
				t.Fatalf("call: %+v %v", r, r.Err)
			}
			calls := obs.byMethod("tools/call")
			if len(calls) != 1 || calls[0].Tool != "echo" || calls[0].Status != 200 || calls[0].Streamed != sse {
				t.Fatalf("call stats: %+v", calls)
			}
			if sse && calls[0].Stream <= 0 {
				t.Error("stream duration not recorded")
			}
			if calls[0].TTFB < 0 || calls[0].Duration < calls[0].TTFB { // Windows clock granularity can make localhost timings 0
				t.Errorf("timings: %+v", calls[0])
			}
			if err := s.Ping(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			_ = s.Close(context.Background()) // idempotent
			f.mu.Lock()
			if len(f.deleted) != 1 || f.deleted[0] != "sess-1" {
				t.Errorf("DELETE not sent: %v", f.deleted)
			}
			f.mu.Unlock()
			if obs.open != 0 || len(obs.connects) != 1 || obs.connects[0].Method != "initialize" {
				t.Errorf("session accounting: open=%d connects=%+v", obs.open, obs.connects)
			}
		})
	}
}

func TestToolIsError(t *testing.T) {
	_, srv := newFake(t, false)
	obs := &collector{}
	s := connect(t, srv.URL, "2025-06-18", obs)
	r := s.CallTool(context.Background(), "fail", nil)
	if !r.IsError || r.Err == nil || r.Err.Type != ErrToolIsError || r.Err.Message != "boom" {
		t.Fatalf("got %+v %+v", r, r.Err)
	}
	c := obs.byMethod("tools/call")
	if len(c) != 1 || c[0].ErrorType != ErrToolIsError {
		t.Fatalf("stats: %+v", c)
	}
}

func TestSessionNotFound(t *testing.T) {
	f, srv := newFake(t, false)
	s := connect(t, srv.URL, "2025-06-18", nil)
	f.mu.Lock()
	f.sessions = map[string]bool{} // server "restarted" / other replica
	f.mu.Unlock()
	_, err := s.ListTools(context.Background())
	if e := AsError(err); e == nil || e.Type != ErrSessionNotFound || e.HTTPStatus != 404 {
		t.Fatalf("got %v", err)
	}
	r := s.CallTool(context.Background(), "echo", nil)
	if r.Err == nil || r.Err.Type != ErrSessionNotFound {
		t.Fatalf("got %+v", r.Err)
	}
}

func TestStatelessMode(t *testing.T) {
	for _, sse := range []bool{false, true} {
		f, srv := newFake(t, true)
		f.sse = sse
		obs := &collector{}
		s := connect(t, srv.URL, ProtocolStateless, obs)
		if !s.Stateless() || s.SessionID() != "" || s.Protocol() != ProtocolStateless {
			t.Fatal("not stateless")
		}
		if len(obs.connects) != 1 || obs.connects[0].Method != "server/discover" {
			t.Fatalf("connects: %+v", obs.connects)
		}
		tools, err := s.ListTools(context.Background())
		if err != nil || len(tools) != 3 {
			t.Fatalf("list: %v %v", tools, err)
		}
		f.mu.Lock()
		if f.lastHdr.Get(HeaderMethod) != "tools/list" || f.lastHdr.Get(HeaderName) != "" {
			t.Errorf("headers: %v", f.lastHdr)
		}
		f.mu.Unlock()
		r := s.CallTool(context.Background(), "echo", map[string]any{"msg": "x"})
		if r.Err != nil {
			t.Fatal(r.Err)
		}
		f.mu.Lock()
		if f.lastHdr.Get(HeaderName) != "echo" {
			t.Errorf("Mcp-Name: %q", f.lastHdr.Get(HeaderName))
		}
		meta := f.lastBody["_meta"].(map[string]any)
		if meta[MetaProtocolVersion] != ProtocolStateless {
			t.Errorf("meta: %v", meta)
		}
		f.mu.Unlock()
		if err := s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		before := f.reqs.Load()
		_ = s.Close(context.Background())
		if f.reqs.Load() != before {
			t.Error("stateless close must not hit the wire")
		}
	}
}

func TestStatelessSkipDiscover(t *testing.T) {
	f, srv := newFake(t, true)
	s := connect(t, srv.URL, ProtocolStateless, nil, func(o *Options) { o.SkipDiscover = true })
	if f.reqs.Load() != 0 {
		t.Fatal("discover sent although skipped")
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNonASCIIName(t *testing.T) {
	if got := EncodeHeaderValue("héllo"); got != "=?base64?aMOpbGxv?=" {
		t.Fatalf("got %q", got)
	}
	if got := EncodeHeaderValue(" x"); !strings.HasPrefix(got, "=?base64?") {
		t.Fatalf("leading space not encoded: %q", got)
	}
	if got := EncodeHeaderValue("plain_name-1"); got != "plain_name-1" {
		t.Fatalf("got %q", got)
	}
	f, srv := newFake(t, true)
	s := connect(t, srv.URL, ProtocolStateless, nil)
	r := s.CallTool(context.Background(), "écho", map[string]any{"msg": "x"})
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(f.lastHdr.Get(HeaderName), "=?base64?") {
		t.Fatalf("Mcp-Name not encoded: %q", f.lastHdr.Get(HeaderName))
	}
}

func TestHeaderMismatch(t *testing.T) {
	_, srv := newFake(t, true)
	// Send a tools/call whose Mcp-Name header differs from params.name.
	obs := &collector{}
	st := &Session{opts: Options{URL: srv.URL, Observer: obs, HTTPClient: http.DefaultClient}, hc: http.DefaultClient,
		protocol: ProtocolStateless, stateless: true}
	st.opts.ClientInfo = Implementation{Name: "t", Version: "1"}
	// Bypass header generation: send a request whose Mcp-Name differs.
	id := st.nextID.Add(1)
	r := st.post(context.Background(), exchange{method: "tools/call", tool: "echo", id: &id, stateless: true,
		protoHdr: ProtocolStateless, nameHdr: "other",
		params: st.statelessParams(ProtocolStateless, map[string]any{"name": "echo"})})
	if r.err == nil || r.err.Type != ErrHeaderMismatch || r.err.Code != CodeHeaderMismatch || r.err.HTTPStatus != 400 {
		t.Fatalf("got %+v", r.err)
	}
	if obs.reqs[0].ErrorType != ErrHeaderMismatch {
		t.Fatalf("stats: %+v", obs.reqs)
	}
}

func TestJSONRPCErrorWith200(t *testing.T) {
	_, srv := newFake(t, false)
	s := connect(t, srv.URL, "2025-06-18", nil)
	r := s.CallTool(context.Background(), "rpcerror", nil)
	if r.Err == nil || r.Err.Type != ErrJSONRPC || r.Err.Code != -32603 {
		t.Fatalf("got %+v", r.Err)
	}
	_, err := s.Request(context.Background(), "nope/method", "", nil)
	if e := AsError(err); e == nil || e.Type != ErrJSONRPC || e.Code != -32601 {
		t.Fatalf("got %v", err)
	}
}

func TestAutoFallsBackToStateful(t *testing.T) {
	_, srv := newFake(t, false)
	obs := &collector{}
	s := connect(t, srv.URL, ProtocolAuto, obs)
	if s.Stateless() || s.Protocol() != DefaultFallbackVersion || s.SessionID() == "" {
		t.Fatalf("expected stateful fallback, got %q stateless=%v", s.Protocol(), s.Stateless())
	}
	if len(obs.byMethod("server/discover")) != 1 || obs.byMethod("server/discover")[0].Status != 400 || obs.byMethod("server/discover")[0].ErrorType != "" {
		t.Fatal("probe not recorded")
	}
	if obs.connects[0].Method != "initialize" || obs.connects[0].ErrorType != "" {
		t.Fatalf("connect stats: %+v", obs.connects)
	}
}

func TestAutoPicksStateless(t *testing.T) {
	_, srv := newFake(t, true)
	s := connect(t, srv.URL, ProtocolAuto, nil)
	if !s.Stateless() || s.Protocol() != ProtocolStateless {
		t.Fatalf("got %q", s.Protocol())
	}
}

func TestAutoDoesNotFallBackOnModernError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 400, rpcErr(1, CodeHeaderMismatch, "mismatch"))
	}))
	defer srv.Close()
	_, err := Connect(context.Background(), Options{URL: srv.URL, Protocol: ProtocolAuto})
	if e := AsError(err); e == nil || e.Type != ErrHeaderMismatch {
		t.Fatalf("got %v", err)
	}
}

func TestTimeout(t *testing.T) {
	f, srv := newFake(t, false)
	f.slow = 300 * time.Millisecond
	obs := &collector{}
	s := connect(t, srv.URL, "2025-06-18", obs, func(o *Options) { o.Timeout = 50 * time.Millisecond })
	r := s.CallTool(context.Background(), "slow", nil)
	if r.Err == nil || r.Err.Type != ErrTimeout {
		t.Fatalf("got %+v", r.Err)
	}
	if c := obs.byMethod("tools/call"); c[0].ErrorType != ErrTimeout || c[0].Status != 0 {
		t.Fatalf("stats %+v", c)
	}
}

func TestCallParallel(t *testing.T) {
	f, srv := newFake(t, false)
	f.sse = true
	f.slow = 100 * time.Millisecond
	obs := &collector{}
	s := connect(t, srv.URL, "2025-06-18", obs)
	calls := []ToolCall{{Name: "slow"}, {Name: "echo", Args: map[string]any{"msg": "a"}}, {Name: "fail"}, {Name: "slow"}, {Name: "slow"}}
	start := time.Now()
	res := s.CallParallel(context.Background(), calls)
	el := time.Since(start)
	if el > 280*time.Millisecond {
		t.Errorf("calls did not run in parallel: %v", el)
	}
	if len(res) != 5 || res[0].Err != nil || !strings.Contains(string(res[1].Content), `"a"`) || res[2].Err.Type != ErrToolIsError {
		t.Fatalf("results: %+v", res)
	}
	if len(obs.byMethod("tools/call")) != 5 {
		t.Fatal("missing stats")
	}
}

func TestAuthErrors(t *testing.T) {
	f, srv := newFake(t, false)
	f.token = "good"
	_, err := Connect(context.Background(), Options{URL: srv.URL, Protocol: "2025-06-18", Auth: BearerAuth{Token: "bad"}})
	if e := AsError(err); e == nil || e.Type != ErrAuth || e.HTTPStatus != 401 {
		t.Fatalf("got %v", err)
	}
	s, err := Connect(context.Background(), Options{URL: srv.URL, Protocol: "2025-06-18", Auth: BearerAuth{Token: "good"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// tokenServer issues tokens valid for ttl and counts fetches.
func tokenServer(t *testing.T, ttl time.Duration, delay time.Duration, basic bool) (*httptest.Server, *atomic.Int64, *atomic.Value) {
	var n atomic.Int64
	var last atomic.Value
	last.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		id, sec, ok := r.BasicAuth()
		if !basic {
			id, sec, ok = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret"), true
		}
		if !ok || id != "mcpload" || sec != "secret" || r.PostForm.Get("grant_type") != "client_credentials" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		time.Sleep(delay)
		k := n.Add(1)
		tok := "tok-" + string(rune('a'+k))
		last.Store(tok)
		writeJSON(w, 200, map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": ttl.Seconds()})
	}))
	t.Cleanup(srv.Close)
	return srv, &n, &last
}

func TestOAuthSingleFlightAndRefresh(t *testing.T) {
	ts, fetches, last := tokenServer(t, 2*time.Second, 50*time.Millisecond, true)
	src := NewTokenSource(OAuthConfig{TokenURL: ts.URL, ClientID: "mcpload", ClientSecret: "secret"})
	var now atomic.Value
	now.Store(time.Now())
	src.now = func() time.Time { return now.Load().(time.Time) }

	obs := &collector{}
	var wg sync.WaitGroup
	toks := make([]string, 50)
	for i := range toks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, err := src.Token(context.Background(), http.DefaultClient, obs)
			if err != nil {
				t.Error(err)
			}
			toks[i] = tok
		}(i)
	}
	wg.Wait()
	if fetches.Load() != 1 {
		t.Fatalf("stampede: %d fetches", fetches.Load())
	}
	for _, tk := range toks {
		if tk != last.Load().(string) {
			t.Fatalf("token mismatch %q", tk)
		}
	}
	if len(obs.tokens) != 1 || obs.tokens[0].Duration < 50*time.Millisecond || obs.tokens[0].Status != 200 {
		t.Fatalf("token stats: %+v", obs.tokens)
	}
	// Still fresh: no refetch.
	_, _ = src.Token(context.Background(), http.DefaultClient, obs)
	if fetches.Load() != 1 {
		t.Fatal("refetched fresh token")
	}
	// Inside the refresh window (last 20% of lifetime): refresh.
	now.Store(now.Load().(time.Time).Add(1700 * time.Millisecond))
	tok, _ := src.Token(context.Background(), http.DefaultClient, obs)
	if fetches.Load() != 2 || tok != last.Load().(string) {
		t.Fatalf("expected refresh, fetches=%d", fetches.Load())
	}
	// A 401 invalidates the token.
	src.Unauthorized("Bearer " + tok)
	_, _ = src.Token(context.Background(), http.DefaultClient, obs)
	if fetches.Load() != 3 {
		t.Fatal("401 did not invalidate token")
	}
}

func TestOAuthEndToEnd(t *testing.T) {
	ts, _, last := tokenServer(t, time.Minute, 0, false)
	src := NewTokenSource(OAuthConfig{TokenURL: ts.URL, ClientID: "mcpload", ClientSecret: "secret", AuthStyle: "post"})
	tok, err := src.Token(context.Background(), http.DefaultClient, nil)
	if err != nil {
		t.Fatal(err)
	}
	f, srv := newFake(t, false)
	f.token = tok
	obs := &collector{}
	s := connect(t, srv.URL, "2025-06-18", obs, func(o *Options) { o.Auth = src })
	if r := s.CallTool(context.Background(), "echo", nil); r.Err != nil {
		t.Fatal(r.Err)
	}
	_ = last
}

func TestOAuthFailure(t *testing.T) {
	ts, _, _ := tokenServer(t, time.Minute, 0, true)
	obs := &collector{}
	src := NewTokenSource(OAuthConfig{TokenURL: ts.URL, ClientID: "mcpload", ClientSecret: "wrong"})
	_, srv := newFake(t, false)
	_, err := Connect(context.Background(), Options{URL: srv.URL, Protocol: "2025-06-18", Auth: src, Observer: obs})
	if e := AsError(err); e == nil || e.Type != ErrAuth {
		t.Fatalf("got %v", err)
	}
	if len(obs.tokens) != 1 || obs.tokens[0].ErrorType != ErrAuth || obs.tokens[0].Status != 401 {
		t.Fatalf("token stats: %+v", obs.tokens)
	}
	if obs.connects[0].ErrorType != ErrAuth {
		t.Fatalf("connect stats: %+v", obs.connects)
	}
}

func TestSharedTokenSource(t *testing.T) {
	cfg := OAuthConfig{TokenURL: "http://x", ClientID: "a", ClientSecret: "b"}
	if SharedTokenSource(cfg) != SharedTokenSource(cfg) {
		t.Fatal("not shared")
	}
	cfg.ClientSecret = "c"
	if SharedTokenSource(cfg) == SharedTokenSource(OAuthConfig{TokenURL: "http://x", ClientID: "a", ClientSecret: "b"}) {
		t.Fatal("different creds shared")
	}
}

func TestReadSSEPicksMatchingID(t *testing.T) {
	stream := ": hi\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":7,\"method\":\"sampling/createMessage\"}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":6,\"result\":{\"x\":1}}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\r\ndata: \"id\":7,\"result\":{\"x\":2}}\r\n\r\n"
	m, err := readSSE(strings.NewReader(stream), "7")
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]int
	_ = json.Unmarshal(m.Result, &r)
	if r["x"] != 2 {
		t.Fatalf("got %s", m.Result)
	}
	if _, err := readSSE(strings.NewReader("data: {}\n\n"), "1"); err == nil {
		t.Fatal("expected error on stream end")
	}
}
