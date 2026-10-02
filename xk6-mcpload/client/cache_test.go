package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// failingTokenServer always answers 401 after delay and counts requests.
func failingTokenServer(t *testing.T, delay time.Duration) (*httptest.Server, *atomic.Int64) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		time.Sleep(delay)
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func fakeClock(start time.Time) (func() time.Time, func(time.Duration)) {
	var now atomic.Value
	now.Store(start)
	return func() time.Time { return now.Load().(time.Time) },
		func(d time.Duration) { now.Store(now.Load().(time.Time).Add(d)) }
}

func TestOAuthNegativeCacheConcurrent(t *testing.T) {
	ts, reqs := failingTokenServer(t, 20*time.Millisecond)
	src := NewTokenSource(OAuthConfig{TokenURL: ts.URL, ClientID: "mcpload", ClientSecret: "wrong"})
	clock, advance := fakeClock(time.Now())
	src.now = clock
	obs := &collector{}

	burst := func() {
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 20; j++ { // each "VU" reconnects repeatedly
					_, err := src.Token(context.Background(), http.DefaultClient, obs)
					e := AsError(err)
					if e == nil || e.Type != ErrAuth || e.HTTPStatus != 401 {
						t.Errorf("want auth/401 error, got %v", err)
						return
					}
				}
			}()
		}
		wg.Wait()
	}

	burst() // 1000 calls inside one backoff window
	if n := reqs.Load(); n < 1 || n > 2 {
		t.Fatalf("token requests within window = %d, want 1..2", n)
	}
	first := reqs.Load()
	if int64(len(obs.tokens)) != first {
		t.Fatalf("token metric samples %d != requests %d", len(obs.tokens), first)
	}
	_, err := src.Token(context.Background(), http.DefaultClient, nil)
	if err == nil || !strings.Contains(err.Error(), "cached") {
		t.Fatalf("expected cached error, got %v", err)
	}

	advance(500 * time.Millisecond) // still inside the 1s default window
	burst()
	if reqs.Load() != first {
		t.Fatalf("re-hit IdP inside window: %d", reqs.Load())
	}

	advance(600 * time.Millisecond) // window over: exactly one new request
	burst()
	if n := reqs.Load() - first; n < 1 || n > 2 {
		t.Fatalf("after window: %d new requests, want 1..2", n)
	}
}

func TestOAuthNegativeCacheConfigurable(t *testing.T) {
	ts, reqs := failingTokenServer(t, 0)

	off := NewTokenSource(OAuthConfig{TokenURL: ts.URL, ClientID: "mcpload", ClientSecret: "wrong", FailureBackoff: -1})
	for i := 0; i < 3; i++ {
		_, _ = off.Token(context.Background(), http.DefaultClient, nil)
	}
	if reqs.Load() != 3 {
		t.Fatalf("disabled cache: %d requests, want 3", reqs.Load())
	}

	reqs.Store(0)
	long := NewTokenSource(OAuthConfig{TokenURL: ts.URL, ClientID: "mcpload", ClientSecret: "wrong", FailureBackoff: 5 * time.Second})
	clock, advance := fakeClock(time.Now())
	long.now = clock
	_, _ = long.Token(context.Background(), http.DefaultClient, nil)
	advance(4 * time.Second)
	_, _ = long.Token(context.Background(), http.DefaultClient, nil)
	if reqs.Load() != 1 {
		t.Fatalf("5s window: %d requests, want 1", reqs.Load())
	}
	advance(2 * time.Second)
	_, _ = long.Token(context.Background(), http.DefaultClient, nil)
	if reqs.Load() != 2 {
		t.Fatalf("after 5s window: %d requests, want 2", reqs.Load())
	}
}

func TestOAuthNegativeCacheNetworkErrorAndRecovery(t *testing.T) {
	// Network error (nothing listening) is cached too.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	src := NewTokenSource(OAuthConfig{TokenURL: deadURL, ClientID: "mcpload", ClientSecret: "secret"})
	clock, advance := fakeClock(time.Now())
	src.now = clock
	obs := &collector{}
	for i := 0; i < 5; i++ {
		if _, err := src.Token(context.Background(), http.DefaultClient, obs); err == nil {
			t.Fatal("expected error")
		}
	}
	if len(obs.tokens) != 1 {
		t.Fatalf("network failure: %d fetches, want 1", len(obs.tokens))
	}

	// After the window a good endpoint recovers and clears the cache.
	good, fetches, _ := tokenServer(t, time.Minute, 0, true)
	src.cfg.TokenURL = good.URL
	advance(1100 * time.Millisecond)
	tok, err := src.Token(context.Background(), http.DefaultClient, obs)
	if err != nil || tok == "" || fetches.Load() != 1 {
		t.Fatalf("recovery: tok=%q err=%v fetches=%d", tok, err, fetches.Load())
	}
	// 401 on a resource still invalidates the good token immediately.
	src.Unauthorized("Bearer " + tok)
	if _, err := src.Token(context.Background(), http.DefaultClient, obs); err != nil || fetches.Load() != 2 {
		t.Fatalf("401 invalidation: err=%v fetches=%d", err, fetches.Load())
	}
}

func TestOAuthSoftRefreshFailureBacksOff(t *testing.T) {
	good, _, _ := tokenServer(t, 10*time.Second, 0, true)
	bad, badReqs := failingTokenServer(t, 0)
	src := NewTokenSource(OAuthConfig{TokenURL: good.URL, ClientID: "mcpload", ClientSecret: "secret"})
	clock, advance := fakeClock(time.Now())
	src.now = clock
	tok, err := src.Token(context.Background(), http.DefaultClient, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.cfg.TokenURL = bad.URL
	advance(9 * time.Second) // in the refresh window, token still valid
	for i := 0; i < 10; i++ {
		got, err := src.Token(context.Background(), http.DefaultClient, nil)
		if err != nil || got != tok {
			t.Fatalf("soft refresh failure should keep old token: %q %v", got, err)
		}
		waitIdle(t, src)
	}
	if badReqs.Load() != 1 {
		t.Fatalf("soft refresh retried %d times inside window", badReqs.Load())
	}
}

func TestProtocolCacheSharedAcrossClients(t *testing.T) {
	for _, stateless := range []bool{true, false} {
		f, srv := newFake(t, stateless)
		pc := &ProtocolCache{}
		obs := &collector{}
		withCache := func(o *Options) { o.ProtocolCache = pc }
		s1 := connect(t, srv.URL, ProtocolAuto, obs, withCache)
		s2 := connect(t, srv.URL, ProtocolAuto, obs, withCache)
		if n := len(obs.byMethod("server/discover")); n != 1 {
			t.Fatalf("stateless=%v: %d probes for two clients, want 1", stateless, n)
		}
		if s1.Protocol() != s2.Protocol() || s1.Stateless() != s2.Stateless() || s2.Stateless() != stateless {
			t.Fatalf("stateless=%v: protocols %q/%q", stateless, s1.Protocol(), s2.Protocol())
		}
		if stateless {
			if f.reqs.Load() != 1 || len(s2.Capabilities) == 0 {
				t.Fatalf("cached stateless connect sent %d requests, caps=%s", f.reqs.Load(), s2.Capabilities)
			}
		} else if s2.SessionID() == "" || len(obs.byMethod("initialize")) != 2 {
			t.Fatalf("cached stateful connect must still initialize: sid=%q", s2.SessionID())
		}
		// The cached session works.
		if r := s2.CallTool(context.Background(), "echo", nil); r.Err != nil {
			t.Fatalf("stateless=%v: %v", stateless, r.Err)
		}
		// Different URL / protocol options are separate entries.
		if _, ok := pc.Load(srv.URL, ProtocolAuto, "2025-06-18"); ok {
			t.Fatal("fallback version not part of key")
		}
	}
}

func TestProtocolCacheOnlyCachesSuccess(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	f := &fakeServer{t: t, stateless: true, sessions: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	pc := &ProtocolCache{}
	obs := &collector{}
	opts := Options{URL: srv.URL, Protocol: ProtocolAuto, Observer: obs, ProtocolCache: pc, Timeout: 5 * time.Second}
	if _, err := Connect(context.Background(), opts); err == nil {
		t.Fatal("expected failure")
	}
	if _, ok := pc.Load(srv.URL, ProtocolAuto, DefaultFallbackVersion); ok {
		t.Fatal("failed connect was cached")
	}
	fail.Store(false)
	if _, err := Connect(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if n := len(obs.byMethod("server/discover")); n != 2 {
		t.Fatalf("expected a re-probe after the failed connect, got %d probes", n)
	}
	if _, ok := pc.Load(srv.URL, ProtocolAuto, DefaultFallbackVersion); !ok {
		t.Fatal("successful connect not cached")
	}
}

func TestNoProtocolCacheProbesEveryConnect(t *testing.T) {
	_, srv := newFake(t, false)
	obs := &collector{}
	connect(t, srv.URL, ProtocolAuto, obs)
	connect(t, srv.URL, ProtocolAuto, obs)
	if n := len(obs.byMethod("server/discover")); n != 2 {
		t.Fatalf("without cache: %d probes, want 2", n)
	}
}

func TestProtocolCacheForgetsRejectedProtocol(t *testing.T) {
	pc := &ProtocolCache{}
	// A cached stateful version that the server (now) rejects.
	rej := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 400, rpcErr(1, CodeUnsupportedProtocolVersion, "unsupported"))
	}))
	t.Cleanup(rej.Close)
	pc.Store(rej.URL, ProtocolAuto, DefaultFallbackVersion, Resolution{Protocol: "1999-01-01"})
	_, err := Connect(context.Background(), Options{URL: rej.URL, Protocol: ProtocolAuto, ProtocolCache: pc})
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := pc.Load(rej.URL, ProtocolAuto, DefaultFallbackVersion); ok {
		t.Fatal("rejected protocol not forgotten")
	}
}
