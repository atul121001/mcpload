package client

import (
	"context"
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

// waitIdle waits until no token fetch is in flight.
func waitIdle(t *testing.T, ts *TokenSource) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ts.mu.Lock()
		busy := ts.inflight != nil
		ts.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("token fetch still in flight")
}

// The fetch runs on a detached context: cancelling the caller that started
// it fails only that caller; everyone else waiting gets the token, and the
// cancellation is not negatively cached.
func TestOAuthLeaderCancelDoesNotFailWaiters(t *testing.T) {
	ts, fetches, last := tokenServer(t, time.Minute, 150*time.Millisecond, true)
	src := NewTokenSource(OAuthConfig{TokenURL: ts.URL, ClientID: "mcpload", ClientSecret: "secret"})

	leaderCtx, cancel := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := src.Token(leaderCtx, http.DefaultClient, nil)
		leaderErr <- err
	}()
	// Make sure the leader owns the in-flight fetch before the waiters join.
	for {
		src.mu.Lock()
		started := src.inflight != nil
		src.mu.Unlock()
		if started {
			break
		}
		time.Sleep(time.Millisecond)
	}
	var wg sync.WaitGroup
	errs := make([]error, 10)
	toks := make([]string, 10)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			toks[i], errs[i] = src.Token(context.Background(), http.DefaultClient, nil)
		}(i)
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-leaderErr; err == nil {
		t.Fatal("cancelled leader should get its own ctx error")
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil || toks[i] != last.Load().(string) {
			t.Fatalf("waiter %d: tok=%q err=%v", i, toks[i], errs[i])
		}
	}
	if fetches.Load() != 1 {
		t.Fatalf("fetches=%d, want 1", fetches.Load())
	}
	// Nothing negatively cached; the token is served from cache.
	if tok, err := src.Token(context.Background(), http.DefaultClient, nil); err != nil || tok != last.Load().(string) {
		t.Fatalf("after: %q %v", tok, err)
	}
}

// hangingTokenServer accepts token requests and answers after delay.
func hangingTokenServer(t *testing.T, delay time.Duration) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		writeJSON(w, 200, map[string]any{"access_token": "late", "expires_in": 60})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOAuthFetchHonoursTimeout(t *testing.T) {
	cases := []struct {
		name       string
		sessionTO  time.Duration
		tokenTO    time.Duration
		wantType   string
		wantWithin time.Duration
	}{
		{"session timeout bounds token fetch", 100 * time.Millisecond, 0, ErrTimeout, time.Second},
		{"oauth timeout shorter than session timeout", 5 * time.Second, 100 * time.Millisecond, ErrTimeout, time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idp := hangingTokenServer(t, 1500*time.Millisecond)
			src := NewTokenSource(OAuthConfig{TokenURL: idp.URL, ClientID: "c", ClientSecret: "s", Timeout: tc.tokenTO})
			_, srv := newFake(t, false)
			start := time.Now()
			_, err := Connect(context.Background(), Options{URL: srv.URL, Protocol: "2025-06-18", Auth: src, Timeout: tc.sessionTO})
			if el := time.Since(start); el > tc.wantWithin {
				t.Fatalf("connect took %v", el)
			}
			if e := AsError(err); e == nil || e.Type != tc.wantType {
				t.Fatalf("got %v, want type %s", err, tc.wantType)
			}
			waitIdle(t, src) // the detached fetch is bounded too
		})
	}
}

func TestOAuthSoftRefreshDoesNotBlock(t *testing.T) {
	ts, fetches, _ := tokenServer(t, 10*time.Second, 300*time.Millisecond, true)
	src := NewTokenSource(OAuthConfig{TokenURL: ts.URL, ClientID: "mcpload", ClientSecret: "secret"})
	clock, advance := fakeClock(time.Now())
	src.now = clock
	tok, err := src.Token(context.Background(), http.DefaultClient, nil)
	if err != nil {
		t.Fatal(err)
	}
	advance(9 * time.Second) // valid but inside the refresh window
	start := time.Now()
	got, err := src.Token(context.Background(), http.DefaultClient, nil)
	if el := time.Since(start); err != nil || got != tok || el > 100*time.Millisecond {
		t.Fatalf("soft refresh blocked %v: %q %v", el, got, err)
	}
	waitIdle(t, src)
	got, _ = src.Token(context.Background(), http.DefaultClient, nil)
	if fetches.Load() != 2 || got == tok {
		t.Fatalf("background refresh not applied: fetches=%d tok=%q", fetches.Load(), got)
	}
}

// negotiatingServer is a modern server that only speaks the stateless
// revision "speaks" (or nothing stateless when speaks == ""), rejecting other
// versions with -32022 and data {"supported": supported}. It also implements
// the stateful handshake for the fallback path.
type negotiatingServer struct {
	speaks    string
	supported []string // nil: -32022 without data
	mu        sync.Mutex
	discovers []string // protocol versions probed with server/discover
	inits     int
}

func (n *negotiatingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var msg struct {
		ID     any            `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	_ = json.Unmarshal(b, &msg)
	switch msg.Method {
	case "server/discover":
		meta, _ := msg.Params["_meta"].(map[string]any)
		v, _ := meta[MetaProtocolVersion].(string)
		n.mu.Lock()
		n.discovers = append(n.discovers, v)
		n.mu.Unlock()
		if n.speaks == "" || v != n.speaks || r.Header.Get(HeaderProtocolVersion) != v {
			e := map[string]any{"code": CodeUnsupportedProtocolVersion, "message": "unsupported protocol version"}
			if n.supported != nil {
				e["data"] = map[string]any{"supported": n.supported, "requested": v}
			}
			writeJSON(w, 400, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": e})
			return
		}
		writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{
			"supportedVersions": n.supported, "capabilities": map[string]any{"tools": map[string]any{}},
		}})
	case "initialize":
		n.mu.Lock()
		n.inits++
		n.mu.Unlock()
		w.Header().Set(HeaderSessionID, "s1")
		writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{
			"protocolVersion": msg.Params["protocolVersion"], "capabilities": map[string]any{},
		}})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	default:
		writeJSON(w, 200, rpcErr(msg.ID, -32601, "method not found"))
	}
}

func TestAutoNegotiatesUnsupportedProtocolVersion(t *testing.T) {
	cases := []struct {
		name          string
		speaks        string
		supported     []string
		wantProtocol  string
		wantStateless bool
		wantDiscovers []string
		wantInits     int
	}{
		{"retries newest offered stateless version", "2026-12-01",
			[]string{"2025-11-25", "2026-09-01", "2026-12-01"}, "2026-12-01", true,
			[]string{"2026-07-28", "2026-12-01"}, 0},
		{"no stateless version offered falls back to initialize", "",
			[]string{"2025-11-25", "2025-06-18"}, DefaultFallbackVersion, false,
			[]string{"2026-07-28"}, 1},
		{"-32022 without data falls back to initialize", "",
			nil, DefaultFallbackVersion, false,
			[]string{"2026-07-28"}, 1},
		{"offered version also rejected does not loop", "",
			[]string{"2026-12-01"}, "", false,
			[]string{"2026-07-28", "2026-12-01"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := &negotiatingServer{speaks: tc.speaks, supported: tc.supported}
			srv := httptest.NewServer(ns)
			t.Cleanup(srv.Close)
			obs := &collector{}
			pc := &ProtocolCache{}
			s, err := Connect(context.Background(), Options{URL: srv.URL, Protocol: ProtocolAuto, Observer: obs, ProtocolCache: pc, Timeout: 5 * time.Second})
			if tc.wantProtocol == "" {
				if e := AsError(err); e == nil || e.Code != CodeUnsupportedProtocolVersion {
					t.Fatalf("want -32022 error, got %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if s.Protocol() != tc.wantProtocol || s.Stateless() != tc.wantStateless {
					t.Fatalf("protocol=%q stateless=%v", s.Protocol(), s.Stateless())
				}
				if res, ok := pc.Load(srv.URL, ProtocolAuto, DefaultFallbackVersion); !ok || res.Protocol != tc.wantProtocol {
					t.Fatalf("cache: %+v %v", res, ok)
				}
			}
			ns.mu.Lock()
			discovers, inits := fmt.Sprint(ns.discovers), ns.inits
			ns.mu.Unlock()
			if discovers != fmt.Sprint(tc.wantDiscovers) || inits != tc.wantInits {
				t.Fatalf("discovers=%v inits=%d", discovers, inits)
			}
			// Negotiation answers are not counted as errors, except the final one.
			probes := obs.byMethod("server/discover")
			for i, p := range probes {
				last := i == len(probes)-1 && tc.wantProtocol == ""
				if (p.ErrorType != "") != last {
					t.Fatalf("probe %d error_type=%q", i, p.ErrorType)
				}
			}
		})
	}
}

func TestFallbackClassification(t *testing.T) {
	data := func(vs ...string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"supported": vs})
		return b
	}
	cases := []struct {
		name     string
		err      *Error
		fallback bool
		rejected bool
		stale    bool
	}{
		{"-32022 offering stateless", &Error{Type: ErrJSONRPC, HTTPStatus: 400, Code: CodeUnsupportedProtocolVersion, Data: data("2026-12-01")}, false, true, true},
		{"-32022 offering only stateful", &Error{Type: ErrJSONRPC, HTTPStatus: 400, Code: CodeUnsupportedProtocolVersion, Data: data("2025-11-25")}, true, true, true},
		{"-32022 without data", &Error{Type: ErrJSONRPC, HTTPStatus: 400, Code: CodeUnsupportedProtocolVersion}, true, true, true},
		{"-32022 with garbage in supported", &Error{Type: ErrJSONRPC, HTTPStatus: 400, Code: CodeUnsupportedProtocolVersion, Data: data("latest")}, true, true, true},
		{"legacy 400", &Error{Type: ErrHTTP, HTTPStatus: 400, Code: -32000}, true, true, true},
		{"method not found", &Error{Type: ErrJSONRPC, HTTPStatus: 200, Code: CodeMethodNotFound}, true, true, true},
		{"tool invalid params over 200", &Error{Type: ErrJSONRPC, HTTPStatus: 200, Code: -32602}, true, true, false},
		{"header mismatch", &Error{Type: ErrHeaderMismatch, HTTPStatus: 400, Code: CodeHeaderMismatch}, false, false, false},
		{"5xx", &Error{Type: ErrHTTP, HTTPStatus: 503}, false, false, false},
		{"timeout", &Error{Type: ErrTimeout}, false, false, false},
		{"auth", &Error{Type: ErrAuth, HTTPStatus: 401}, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldFallback(tc.err); got != tc.fallback {
				t.Errorf("shouldFallback=%v", got)
			}
			if got := protocolRejected(tc.err); got != tc.rejected {
				t.Errorf("protocolRejected=%v", got)
			}
			if got := staleProtocolError(tc.err); got != tc.stale {
				t.Errorf("staleProtocolError=%v", got)
			}
		})
	}
}

func TestInitializedFailureDeletesSession(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"500", http.StatusInternalServerError},
		{"400", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var deleted []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					mu.Lock()
					deleted = append(deleted, r.Header.Get(HeaderSessionID))
					mu.Unlock()
					return
				}
				b, _ := io.ReadAll(r.Body)
				if strings.Contains(string(b), `"notifications/initialized"`) {
					w.WriteHeader(tc.status)
					return
				}
				w.Header().Set(HeaderSessionID, "leak-me")
				writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"protocolVersion": "2025-06-18"}})
			}))
			t.Cleanup(srv.Close)
			obs := &collector{}
			_, err := Connect(context.Background(), Options{URL: srv.URL, Protocol: "2025-06-18", Observer: obs, Timeout: 5 * time.Second})
			if e := AsError(err); e == nil || e.HTTPStatus != tc.status {
				t.Fatalf("got %v", err)
			}
			mu.Lock()
			gotDeleted := fmt.Sprint(deleted)
			mu.Unlock()
			if gotDeleted != "[leak-me]" {
				t.Fatalf("deleted=%v", gotDeleted)
			}
			if d := obs.byMethod("DELETE"); len(d) != 1 || d[0].ErrorType != "" {
				t.Fatalf("DELETE stats: %+v", d)
			}
			if obs.open != 0 {
				t.Fatalf("open=%d", obs.open)
			}
		})
	}
}

func TestEncodeHeaderValue(t *testing.T) {
	cases := []struct {
		in      string
		encoded bool
	}{
		{"", false},
		{"echo", false},
		{"a b", false},
		{"=?base64?", false}, // prefix only: not an encoded-looking value
		{"?=", false},
		{"x=?base64?eA==?=", false},
		{"=?base64?ZWNobw==?=", true}, // literal that looks encoded
		{"=?BASE64?eA==?=", true},
		{"=?base64??=", true},
		{" lead", true},
		{"trail\t", true},
		{"héllo", true},
		{"tab\tin", true},
	}
	for _, tc := range cases {
		want := tc.in
		if tc.encoded {
			want = "=?base64?" + base64.StdEncoding.EncodeToString([]byte(tc.in)) + "?="
		}
		if got := EncodeHeaderValue(tc.in); got != want {
			t.Errorf("EncodeHeaderValue(%q)=%q, want %q", tc.in, got, want)
		}
	}
}

// A literal tool name that looks encoded must survive the round trip.
func TestLiteralBase64LookingName(t *testing.T) {
	_, srv := newFake(t, true)
	s := connect(t, srv.URL, ProtocolStateless, nil)
	if r := s.CallTool(context.Background(), "=?base64?ZWNobw==?=", nil); r.Err != nil {
		t.Fatal(r.Err)
	}
}

func TestStaleCachedStatelessResolutionForgotten(t *testing.T) {
	cases := []struct {
		name   string
		handle func(w http.ResponseWriter, id any)
		forget bool
	}{
		{"legacy 400", func(w http.ResponseWriter, id any) {
			writeJSON(w, 400, rpcErr(nil, -32000, "Bad Request: no Mcp-Session-Id header"))
		}, true},
		{"unsupported protocol version", func(w http.ResponseWriter, id any) {
			writeJSON(w, 400, rpcErr(id, CodeUnsupportedProtocolVersion, "unsupported"))
		}, true},
		{"method not found", func(w http.ResponseWriter, id any) {
			writeJSON(w, 200, rpcErr(id, CodeMethodNotFound, "method not found"))
		}, true},
		{"tool-level invalid params", func(w http.ResponseWriter, id any) {
			writeJSON(w, 200, rpcErr(id, -32602, "invalid params"))
		}, false},
		{"server error", func(w http.ResponseWriter, id any) { w.WriteHeader(503) }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var m struct {
					ID any `json:"id"`
				}
				_ = json.NewDecoder(r.Body).Decode(&m)
				tc.handle(w, m.ID)
			}))
			t.Cleanup(srv.Close)
			pc := &ProtocolCache{}
			pc.Store(srv.URL, ProtocolAuto, DefaultFallbackVersion, Resolution{Protocol: ProtocolStateless, Stateless: true})
			s := connect(t, srv.URL, ProtocolAuto, nil, func(o *Options) { o.ProtocolCache = pc })
			if r := s.CallTool(context.Background(), "echo", nil); r.Err == nil {
				t.Fatal("expected error")
			}
			_, ok := pc.Load(srv.URL, ProtocolAuto, DefaultFallbackVersion)
			if ok == tc.forget {
				t.Fatalf("cache entry present=%v, want forgotten=%v", ok, tc.forget)
			}
		})
	}
}

// A session created by a full negotiation (not from the cache) does not touch
// the cache on request errors; only the cache-derived session does.
func TestFreshSessionDoesNotForget(t *testing.T) {
	f := &fakeServer{t: t, stateless: true, sessions: map[string]bool{}}
	var legacy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if legacy.Load() {
			writeJSON(w, 400, rpcErr(nil, -32000, "Bad Request: no Mcp-Session-Id header"))
			return
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	pc := &ProtocolCache{}
	s := connect(t, srv.URL, ProtocolAuto, nil, func(o *Options) { o.ProtocolCache = pc })
	legacy.Store(true) // server "downgrades"; this session itself was probed
	if r := s.CallTool(context.Background(), "echo", nil); r.Err == nil {
		t.Fatal("expected error")
	}
	if _, ok := pc.Load(srv.URL, ProtocolAuto, DefaultFallbackVersion); !ok {
		t.Fatal("entry from a fresh negotiation should be kept")
	}
}

// TTFB is recorded (race-free) when headers arrive but the body times out.
func TestTTFBOnErrorPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Headers after a few ms, so that TTFB is above the clock resolution
		// (it measured 0 on Windows when they came back at once).
		time.Sleep(5 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	obs := &collector{}
	s := connect(t, srv.URL, ProtocolStateless, obs, func(o *Options) { o.SkipDiscover = true; o.Timeout = 100 * time.Millisecond })
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.CallTool(context.Background(), "echo", nil) }()
	}
	wg.Wait()
	for _, r := range obs.byMethod("tools/call") {
		if r.ErrorType != ErrTimeout || r.TTFB <= 0 || r.TTFB > r.Duration {
			t.Fatalf("stats: %+v", r)
		}
	}
}

func TestValidateProtocol(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"", true}, {"auto", true}, {"2026-07-28", true}, {"2025-06-18", true},
		{"latest", false}, {"AUTO", false}, {"2025-6-18", false}, {"2025-13-01", false},
		{"2025-06-18 ", false}, {"stateless", false},
	}
	for _, tc := range cases {
		if err := ValidateProtocol(tc.in); (err == nil) != tc.ok {
			t.Errorf("ValidateProtocol(%q)=%v", tc.in, err)
		}
	}
	var sent atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sent.Add(1) }))
	t.Cleanup(srv.Close)
	for _, o := range []Options{{URL: srv.URL, Protocol: "latest"}, {URL: srv.URL, FallbackVersion: "v1"}} {
		if _, err := Connect(context.Background(), o); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Fatalf("Connect(%+v)=%v", o, err)
		}
	}
	if sent.Load() != 0 {
		t.Fatal("invalid options reached the server")
	}
}
