package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// skewBuild is one replica of a rolling deploy: "old" speaks only the
// stateful protocol (answers a stateless request with 400 -32000, like the TS
// demo server), "new" speaks only 2026-07-28 (rejects initialize and any
// older protocol header with -32022, data.supported = newSupports).
type skewBuild struct {
	name        string
	stateless   bool
	newSupports []string
}

func (b skewBuild) serve(w http.ResponseWriter, r *http.Request, method string, id any, params map[string]any) {
	w.Header().Set("X-Served-By", b.name)
	if b.stateless {
		v := r.Header.Get(HeaderProtocolVersion)
		if method == "initialize" || !IsStateless(v) {
			writeJSON(w, 400, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{
				"code": CodeUnsupportedProtocolVersion, "message": "unsupported protocol version",
				"data": map[string]any{"supported": b.newSupports},
			}})
			return
		}
		switch method {
		case "server/discover":
			writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"supportedVersions": []string{ProtocolStateless}, "capabilities": map[string]any{},
			}})
		case "tools/call":
			writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "ok"}},
			}})
		default:
			writeJSON(w, 200, rpcErr(id, CodeMethodNotFound, "method not found"))
		}
		return
	}
	switch {
	case method == "initialize":
		w.Header().Set(HeaderSessionID, "s-"+b.name)
		writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
			"protocolVersion": params["protocolVersion"], "capabilities": map[string]any{},
		}})
	case r.Header.Get(HeaderSessionID) == "":
		writeJSON(w, 400, rpcErr(nil, -32000, "Bad Request: no Mcp-Session-Id header"))
	case method == "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	default:
		writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
	}
}

// skewLB sends the n-th request to route[n] (the last entry repeats), like a
// round-robin load balancer whose order the test controls.
type skewLB struct {
	route []skewBuild
	mu    sync.Mutex
	seen  []string // "replica method" per request
}

func (lb *skewLB) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var msg struct {
		ID     any            `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	_ = json.Unmarshal(b, &msg)
	lb.mu.Lock()
	rep := lb.route[min(len(lb.seen), len(lb.route)-1)]
	lb.seen = append(lb.seen, rep.name+" "+msg.Method)
	lb.mu.Unlock()
	rep.serve(w, r, msg.Method, msg.ID, msg.Params)
}

func TestAutoUnderVersionSkew(t *testing.T) {
	old := skewBuild{name: "old"}
	newB := skewBuild{name: "new", stateless: true, newSupports: []string{ProtocolStateless}}
	newOnlyStateful := skewBuild{name: "new", stateless: true, newSupports: []string{"2025-06-18"}}
	cases := []struct {
		name         string
		route        []skewBuild
		wantProtocol string // "" = connect fails
		wantServedBy string
		wantSeen     string
	}{
		{"probe on new build", []skewBuild{newB}, ProtocolStateless, "new", "[new server/discover]"},
		{"probe and handshake on old build", []skewBuild{old}, DefaultFallbackVersion, "old",
			"[old server/discover old initialize old notifications/initialized]"},
		{"initialize rejected by new build retries the offered stateless version", []skewBuild{old, newB, newB},
			ProtocolStateless, "new", "[old server/discover new initialize new server/discover]"},
		{"retry lands on old build again: fails, no loop", []skewBuild{old, newB, old},
			"", "", "[old server/discover new initialize old server/discover]"},
		{"-32022 offering no stateless version is not retried", []skewBuild{old, newOnlyStateful},
			"", "", "[old server/discover new initialize]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lb := &skewLB{route: tc.route}
			srv := httptest.NewServer(lb)
			t.Cleanup(srv.Close)
			s, err := Connect(context.Background(), Options{URL: srv.URL, Protocol: ProtocolAuto, Timeout: 5 * time.Second})
			if tc.wantProtocol == "" {
				if err == nil {
					t.Fatalf("connect succeeded with protocol %q", s.Protocol())
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if s.Protocol() != tc.wantProtocol || s.ServedBy() != tc.wantServedBy {
					t.Fatalf("protocol=%q servedBy=%q", s.Protocol(), s.ServedBy())
				}
			}
			lb.mu.Lock()
			seen := fmt.Sprint(lb.seen)
			lb.mu.Unlock()
			if seen != tc.wantSeen {
				t.Fatalf("requests %s, want %s", seen, tc.wantSeen)
			}
		})
	}
}

func TestServedBy(t *testing.T) {
	newB := skewBuild{name: "new", stateless: true, newSupports: []string{ProtocolStateless}}
	// A stateless session whose calls alternate between the new and the old build.
	lb := &skewLB{route: []skewBuild{newB, newB, {name: "old"}}}
	srv := httptest.NewServer(lb)
	t.Cleanup(srv.Close)
	s, err := Connect(context.Background(), Options{URL: srv.URL, Protocol: ProtocolAuto, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if s.ServedBy() != "new" {
		t.Fatalf("session servedBy %q", s.ServedBy())
	}
	ok := s.CallTool(context.Background(), "fast", nil)
	if ok.Err != nil || ok.ServedBy != "new" {
		t.Fatalf("call 1: err=%v servedBy=%q", ok.Err, ok.ServedBy)
	}
	bad := s.CallTool(context.Background(), "fast", nil)
	if bad.Err == nil || bad.Err.HTTPStatus != 400 || bad.ServedBy != "old" || bad.Err.ServedBy != "old" {
		t.Fatalf("call 2: %+v / %+v", bad, bad.Err)
	}
	_, lerr := s.ListTools(context.Background())
	if e := AsError(lerr); e == nil || e.ServedBy != "old" {
		t.Fatalf("tools/list error %v", lerr)
	}
}

func TestServedByCustomHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Served-By", "wrong")
		w.Header().Set("X-Upstream", "10.0.0.7:3000")
		writeJSON(w, 404, rpcErr(nil, -32601, "nope"))
	}))
	t.Cleanup(srv.Close)
	_, err := Connect(context.Background(), Options{URL: srv.URL, Protocol: ProtocolStateless, ServedByHeader: "X-Upstream", Timeout: 5 * time.Second})
	if e := AsError(err); e == nil || e.ServedBy != "10.0.0.7:3000" {
		t.Fatalf("got %v (servedBy %q)", err, AsError(err).ServedBy)
	}
}

func TestServedByEmptyWithoutResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	_, err := Connect(context.Background(), Options{URL: srv.URL, Protocol: ProtocolStateless, Timeout: 50 * time.Millisecond})
	if e := AsError(err); e == nil || e.Type != ErrTimeout || e.ServedBy != "" {
		t.Fatalf("got %v", err)
	}
}
