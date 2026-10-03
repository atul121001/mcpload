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

// cancelServer is a stateful (or stateless) server whose tools/call answers
// over SSE after delay. What it does when notifications/cancelled arrives
// depends on mode: "ignore" still sends the response after delay (a late
// response), "close" ends the stream without a response, "silent" sends
// nothing and keeps the stream open until the client goes away.
type cancelServer struct {
	stateless  bool
	mode       string
	delay      time.Duration
	failCancel bool // answer notifications/cancelled with 500

	mu       sync.Mutex
	callIDs  []float64
	metas    []any // params._meta of each tools/call
	cancels  []map[string]any
	hdrs     []http.Header
	aborted  map[float64]chan struct{}
	gone     int // tools/call streams whose client disconnected before the response
	cancelCh chan struct{}
}

func newCancelServer(t *testing.T, stateless bool, mode string, delay time.Duration) (*cancelServer, *httptest.Server) {
	f := &cancelServer{stateless: stateless, mode: mode, delay: delay, aborted: map[float64]chan struct{}{}, cancelCh: make(chan struct{}, 16)}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *cancelServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set(DefaultServedByHeader, "replica-1")
	b, _ := io.ReadAll(r.Body)
	var msg map[string]any
	_ = json.Unmarshal(b, &msg)
	method, _ := msg["method"].(string)
	params, _ := msg["params"].(map[string]any)
	switch method {
	case "initialize":
		w.Header().Set(HeaderSessionID, "s1")
		writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": map[string]any{
			"protocolVersion": params["protocolVersion"], "capabilities": map[string]any{"tools": map[string]any{}},
		}})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "server/discover":
		writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": map[string]any{
			"supportedVersions": []string{ProtocolStateless}, "capabilities": map[string]any{"tools": map[string]any{}},
		}})
	case "notifications/cancelled":
		f.mu.Lock()
		f.cancels = append(f.cancels, params)
		f.hdrs = append(f.hdrs, r.Header.Clone())
		id, _ := params["requestId"].(float64)
		if ch := f.aborted[id]; ch != nil {
			close(ch)
			delete(f.aborted, id)
		}
		f.mu.Unlock()
		f.cancelCh <- struct{}{}
		if f.failCancel {
			writeJSON(w, 500, rpcErr(nil, -32603, "boom"))
			return
		}
		w.WriteHeader(http.StatusAccepted)
	case "tools/call":
		id, _ := msg["id"].(float64)
		abort := make(chan struct{})
		f.mu.Lock()
		f.callIDs = append(f.callIDs, id)
		f.metas = append(f.metas, params["_meta"])
		f.aborted[id] = abort
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		delay := f.delay
		if a, ok := params["arguments"].(map[string]any); ok {
			if ms, ok := a["ms"].(float64); ok {
				delay = time.Duration(ms) * time.Millisecond
			}
		}
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-abort:
			switch f.mode {
			case "close":
				return
			case "silent":
				<-r.Context().Done()
				return
			}
			<-t.C // ignore: finish the work and answer anyway
		case <-r.Context().Done():
			f.mu.Lock()
			f.gone++
			f.mu.Unlock()
			return
		}
		resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "done"}}}})
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", resp)
		w.(http.Flusher).Flush()
	default:
		writeJSON(w, 200, rpcErr(msg["id"], -32601, "method not found"))
	}
}

// cancelCollector is a collector that also receives cancel events.
type cancelCollector struct {
	collector
	ch chan CancelStats
}

func newCancelCollector() *cancelCollector { return &cancelCollector{ch: make(chan CancelStats, 16)} }

func (c *cancelCollector) OnCancel(s CancelStats) { c.ch <- s }

func (c *cancelCollector) next(t *testing.T) CancelStats {
	t.Helper()
	select {
	case s := <-c.ch:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no cancel event")
	}
	return CancelStats{}
}

func TestCancelStatefulLateResponse(t *testing.T) {
	f, srv := newCancelServer(t, false, "ignore", 250*time.Millisecond)
	obs := newCancelCollector()
	s := connect(t, srv.URL, "2025-11-25", obs, func(o *Options) { o.CancelWait = time.Second })
	start := time.Now()
	r := s.CallToolWith(context.Background(), "slow", nil, CallOptions{CancelAfter: 50 * time.Millisecond})
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Errorf("the call did not stop waiting at the cancel: %v", el)
	}
	if !r.Cancelled || r.Err == nil || r.Err.Type != ErrCancelled {
		t.Fatalf("result %+v", r)
	}
	f.mu.Lock()
	if len(f.cancels) != 1 || len(f.callIDs) != 1 {
		t.Fatalf("cancels %v, calls %v", f.cancels, f.callIDs)
	}
	c, h := f.cancels[0], f.hdrs[0]
	if c["requestId"] != f.callIDs[0] || c["reason"] == "" || c["reason"] == nil {
		t.Errorf("notification params %v, call id %v", c, f.callIDs[0])
	}
	if h.Get(HeaderSessionID) != "s1" || h.Get(HeaderProtocolVersion) != "2025-11-25" {
		t.Errorf("notification headers %v", h)
	}
	f.mu.Unlock()

	cs := obs.next(t)
	if cs.Outcome != CancelOutcomeLateResponse || cs.Reason != CancelReasonClient || cs.Tool != "slow" || cs.Status != 202 {
		t.Fatalf("cancel %+v", cs)
	}
	if cs.LateAfter < 100*time.Millisecond || cs.LateAfter > 900*time.Millisecond {
		t.Errorf("late after %v, want ~200ms", cs.LateAfter)
	}
	if cs.Duration < 0 || cs.Duration > cs.LateAfter { // may read 0 with a coarse (Windows) clock
		t.Errorf("cancel duration %v", cs.Duration)
	}
	calls := obs.byMethod("tools/call")
	if len(calls) != 1 || calls[0].ErrorType != ErrCancelled || calls[0].Status != 200 {
		t.Errorf("tools/call stats %+v", calls)
	}
	if n := obs.byMethod("notifications/cancelled"); len(n) != 1 || n[0].ErrorType != "" || n[0].Status != 202 {
		t.Errorf("notification stats %+v", n)
	}
}

func TestCancelStatefulNoLateResponse(t *testing.T) {
	for _, mode := range []string{"close", "silent"} {
		t.Run(mode, func(t *testing.T) {
			f, srv := newCancelServer(t, false, mode, 2*time.Second)
			obs := newCancelCollector()
			s := connect(t, srv.URL, "2025-11-25", obs, func(o *Options) { o.CancelWait = 300 * time.Millisecond })
			r := s.CallToolWith(context.Background(), "slow", nil, CallOptions{CancelAfter: 30 * time.Millisecond})
			if !r.Cancelled {
				t.Fatalf("result %+v", r)
			}
			cs := obs.next(t)
			if cs.Outcome != CancelOutcomeCancelled {
				t.Fatalf("cancel %+v", cs)
			}
			waited := time.Since(cs.Start)
			if mode == "silent" && waited < 300*time.Millisecond {
				t.Errorf("silent server: cancel reported after %v, want >= CancelWait", waited)
			}
			if mode == "close" && waited > 250*time.Millisecond {
				t.Errorf("closed stream: cancel reported after %v, want at once", waited)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.cancels) != 1 {
				t.Errorf("cancels %v", f.cancels)
			}
		})
	}
}

func TestCancelStateless(t *testing.T) {
	f, srv := newCancelServer(t, true, "ignore", time.Second)
	obs := newCancelCollector()
	s := connect(t, srv.URL, ProtocolStateless, obs)
	r := s.CallToolWith(context.Background(), "slow", nil, CallOptions{CancelAfter: 50 * time.Millisecond})
	if !r.Cancelled {
		t.Fatalf("result %+v", r)
	}
	cs := obs.next(t)
	if cs.Outcome != CancelOutcomeCancelled || cs.Status != 0 || cs.Protocol != ProtocolStateless {
		t.Fatalf("cancel %+v", cs)
	}
	// Closing the stream is the cancellation: the server sees the client go
	// away, and no notification is sent.
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		gone, n := f.gone, len(f.cancels)
		f.mu.Unlock()
		if n != 0 {
			t.Fatalf("stateless client sent notifications/cancelled")
		}
		if gone == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not see the stream closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := obs.byMethod("notifications/cancelled"); len(n) != 0 {
		t.Errorf("notification stats %+v", n)
	}
}

func TestCancelCompletedFirst(t *testing.T) {
	f, srv := newCancelServer(t, false, "ignore", 10*time.Millisecond)
	obs := newCancelCollector()
	s := connect(t, srv.URL, "2025-11-25", obs)
	r := s.CallToolWith(context.Background(), "slow", nil, CallOptions{CancelAfter: 500 * time.Millisecond})
	if r.Err != nil || r.Cancelled {
		t.Fatalf("result %+v", r)
	}
	if cs := obs.next(t); cs.Outcome != CancelOutcomeCompleted || cs.Duration != 0 {
		t.Fatalf("cancel %+v", cs)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cancels) != 0 {
		t.Errorf("sent a cancel for a completed call")
	}
}

func TestCancelSendFailed(t *testing.T) {
	f, srv := newCancelServer(t, false, "silent", 2*time.Second)
	f.failCancel = true
	obs := newCancelCollector()
	s := connect(t, srv.URL, "2025-11-25", obs)
	r := s.CallToolWith(context.Background(), "slow", nil, CallOptions{CancelAfter: 30 * time.Millisecond})
	if !r.Cancelled {
		t.Fatalf("result %+v", r)
	}
	cs := obs.next(t)
	if cs.Outcome != CancelOutcomeSendFailed || cs.ErrorType != ErrHTTP || cs.Status != 500 {
		t.Fatalf("cancel %+v", cs)
	}
	if n := obs.byMethod("notifications/cancelled"); len(n) != 1 || n[0].ErrorType != ErrHTTP {
		t.Errorf("notification stats %+v", n)
	}
}

func TestCancelOnTimeout(t *testing.T) {
	f, srv := newCancelServer(t, false, "close", time.Second)
	obs := newCancelCollector()
	s := connect(t, srv.URL, "2025-11-25", obs, func(o *Options) { o.Timeout = 80 * time.Millisecond })
	r := s.CallTool(context.Background(), "slow", nil)
	if r.Err == nil || r.Err.Type != ErrTimeout || r.Cancelled {
		t.Fatalf("result %+v", r)
	}
	cs := obs.next(t)
	if cs.Reason != CancelReasonTimeout || cs.Outcome != CancelOutcomeCancelled || cs.Status != 202 {
		t.Fatalf("cancel %+v", cs)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cancels) != 1 || f.cancels[0]["requestId"] != f.callIDs[0] {
		t.Errorf("cancels %v, calls %v", f.cancels, f.callIDs)
	}
}

func TestCallParallelCancel(t *testing.T) {
	f, srv := newCancelServer(t, false, "close", 0)
	obs := newCancelCollector()
	s := connect(t, srv.URL, "2025-11-25", obs)
	calls := []ToolCall{
		{Name: "slow", Args: map[string]any{"ms": 400}, CallOptions: CallOptions{CancelAfter: 40 * time.Millisecond}},
		{Name: "slow", Args: map[string]any{"ms": 20}},
		{Name: "slow", Args: map[string]any{"ms": 400}, CallOptions: CallOptions{CancelAfter: 60 * time.Millisecond}},
	}
	start := time.Now()
	res := s.CallParallel(context.Background(), calls)
	if el := time.Since(start); el > 300*time.Millisecond {
		t.Errorf("CallParallel waited for the cancelled calls: %v", el)
	}
	if !res[0].Cancelled || res[1].Err != nil || !res[2].Cancelled {
		t.Fatalf("results %+v", res)
	}
	obs.next(t)
	obs.next(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cancels) != 2 {
		t.Errorf("cancels %v", f.cancels)
	}
}

// A call with both _meta and CancelAfter sends the meta, is cancelled, and
// still reports the replica whose response headers had arrived.
func TestCancelWithMetaKeepsServedBy(t *testing.T) {
	f, srv := newCancelServer(t, false, "close", time.Second)
	s := connect(t, srv.URL, "2025-11-25", newCancelCollector())
	r := s.CallToolMetaWith(context.Background(), "slow", nil, map[string]any{"io.mcpload/callId": "c1"},
		CallOptions{CancelAfter: 100 * time.Millisecond})
	if !r.Cancelled || r.Err == nil || r.Err.Type != ErrCancelled {
		t.Fatalf("result %+v", r)
	}
	if r.ServedBy != "replica-1" || r.Err.ServedBy != "replica-1" {
		t.Errorf("servedBy %q / %q, want replica-1", r.ServedBy, r.Err.ServedBy)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.metas) != 1 {
		t.Fatalf("metas %v", f.metas)
	}
	if m, _ := f.metas[0].(map[string]any); m["io.mcpload/callId"] != "c1" {
		t.Errorf("_meta %v", f.metas[0])
	}
}
