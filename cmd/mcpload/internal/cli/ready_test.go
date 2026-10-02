package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var fastWait = readyWait{Interval: 5 * time.Millisecond, MaxDelay: 20 * time.Millisecond, LogEvery: time.Hour}

func quiet(string, ...any) {}

func rpcBody(r *http.Request) map[string]any {
	var m map[string]any
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &m)
	return m
}

func TestWaitReadyAfterFailures(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 3 {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"supportedVersions":["2026-07-28"]}}`)
	}))
	defer srv.Close()
	w := fastWait
	w.Max = 5 * time.Second
	var logs []string
	err := waitReady(context.Background(), &readyProbe{URL: srv.URL, Protocol: "2026-07-28"}, w,
		func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	if err != nil {
		t.Fatal(err)
	}
	if n.Load() != 4 {
		t.Errorf("attempts = %d, want 4", n.Load())
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "ready after") {
		t.Errorf("logs = %q", logs)
	}
}

func TestWaitReadyHandshakes(t *testing.T) {
	cases := []struct {
		name, protocol string
		handler        http.HandlerFunc
	}{
		{"json initialize", "2025-06-18", func(w http.ResponseWriter, r *http.Request) {
			if m := rpcBody(r); m["method"] != "initialize" {
				t.Errorf("method = %v", m["method"])
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`)
		}},
		{"sse initialize", "2025-06-18", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\"}\n\n")
			fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n")
		}},
		{"stateless discover", "2026-07-28", func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Mcp-Method") != "server/discover" || r.Header.Get("Mcp-Protocol-Version") != "2026-07-28" {
				t.Errorf("headers = %v", r.Header)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		}},
		{"auto falls back to initialize", "auto", func(w http.ResponseWriter, r *http.Request) {
			if rpcBody(r)["method"] == "server/discover" {
				http.Error(w, "Bad Request: No valid session ID provided", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.handler)
			defer srv.Close()
			w := fastWait
			w.Max = 2 * time.Second
			if err := waitReady(context.Background(), &readyProbe{URL: srv.URL, Protocol: c.protocol}, w, quiet); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWaitReadyAuth(t *testing.T) {
	var n atomic.Int32
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		auth.Store(r.Header.Get("Authorization") + "|" + r.Header.Get("X-Tenant"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	p, err := newReadyProbe(map[string]string{"MCP_URL": srv.URL, "MCP_TOKEN": "t0k", "MCP_HEADERS": `{"X-Tenant":"load"}`})
	if err != nil {
		t.Fatal(err)
	}
	w := fastWait
	w.Max = 5 * time.Second
	err = waitReady(context.Background(), p, w, quiet)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("err = %v", err)
	}
	if n.Load() != 1 {
		t.Errorf("attempts = %d, want 1 (401 stops immediately)", n.Load())
	}
	if got := auth.Load(); got != "Bearer t0k|load" {
		t.Errorf("auth headers = %v", got)
	}

	// With OAuth the probe sends no token, so a 401 means the server is up.
	p, _ = newReadyProbe(map[string]string{"MCP_URL": srv.URL, "OAUTH_TOKEN_URL": "http://idp/token", "MCP_TOKEN": "ignored"})
	if err := waitReady(context.Background(), p, w, quiet); err != nil {
		t.Fatalf("oauth: %v", err)
	}
	if got := auth.Load(); got != "|" {
		t.Errorf("oauth probe sent auth headers %v", got)
	}
}

func TestWaitReadyTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String() + "/mcp"
	ln.Close() // nothing listens: connection refused
	w := fastWait
	w.Max = 3 * time.Second
	start := time.Now()
	err = waitReady(context.Background(), &readyProbe{URL: url, Protocol: "auto"}, w, quiet)
	if err == nil || !strings.Contains(err.Error(), "not ready after 3s") || !strings.Contains(err.Error(), "last: connection refused") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Errorf("took %s", d)
	}

	// A JSON-RPC error is not readiness, and the error says why.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"warming up"}}`)
	}))
	defer srv.Close()
	w.Max = 300 * time.Millisecond
	err = waitReady(context.Background(), &readyProbe{URL: srv.URL, Protocol: "2025-06-18"}, w, quiet)
	if err == nil || !strings.Contains(err.Error(), "warming up") {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitReadyCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	w := fastWait
	w.Max = time.Minute
	err := waitReady(ctx, &readyProbe{URL: srv.URL, Protocol: "auto"}, w, quiet)
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitReadyDeletesSession(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			deleted = append(deleted, r.Header.Get("Mcp-Session-Id")+"|"+r.Header.Get("Authorization"))
			mu.Unlock()
			return
		}
		w.Header().Set("Mcp-Session-Id", "sess-1")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}`)
	}))
	defer srv.Close()
	p, _ := newReadyProbe(map[string]string{"MCP_URL": srv.URL, "MCP_PROTOCOL": "2025-11-25", "MCP_TOKEN": "x"})
	w := fastWait
	w.Max = 2 * time.Second
	if err := waitReady(context.Background(), p, w, quiet); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "sess-1|Bearer x" {
		t.Errorf("DELETE requests = %q", deleted)
	}
}

func TestNewReadyProbeBadHeaders(t *testing.T) {
	if _, err := newReadyProbe(map[string]string{"MCP_HEADERS": "[1]"}); err == nil {
		t.Error("want error for non-object MCP_HEADERS")
	}
}
