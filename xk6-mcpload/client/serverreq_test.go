package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// clientReqServer is a stateful (or stateless) server whose tools/call
// streams first send the server-to-client requests in send, then wait for
// the client's answers (stateful) and finish with a result that echoes them.
type clientReqServer struct {
	t         *testing.T
	stateless bool
	send      []string // methods sent as server requests on each tools/call stream

	mu       sync.Mutex
	initCaps map[string]any
	answers  []map[string]any // JSON-RPC responses POSTed by the client
	answerCh chan map[string]any
	hdrs     []http.Header
}

func newClientReqServer(t *testing.T, stateless bool, send ...string) (*clientReqServer, *httptest.Server) {
	f := &clientReqServer{t: t, stateless: stateless, send: send, answerCh: make(chan map[string]any, 16)}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *clientReqServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusOK)
		return
	}
	b, _ := io.ReadAll(r.Body)
	var msg map[string]any
	if err := json.Unmarshal(b, &msg); err != nil {
		writeJSON(w, 400, rpcErr(nil, -32700, "parse error"))
		return
	}
	method, _ := msg["method"].(string)
	params, _ := msg["params"].(map[string]any)
	if method == "" { // a JSON-RPC response from the client
		f.mu.Lock()
		f.answers = append(f.answers, msg)
		f.hdrs = append(f.hdrs, r.Header.Clone())
		f.mu.Unlock()
		f.answerCh <- msg
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch method {
	case "initialize":
		f.mu.Lock()
		f.initCaps, _ = params["capabilities"].(map[string]any)
		f.mu.Unlock()
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
	case "tools/call":
		f.toolCall(w, r, msg["id"])
	default:
		writeJSON(w, 200, rpcErr(msg["id"], -32601, "method not found"))
	}
}

func (f *clientReqServer) toolCall(w http.ResponseWriter, r *http.Request, id any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fl := w.(http.Flusher)
	fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
	for i, m := range f.send {
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"srv-%d\",\"method\":%q,\"params\":{\"x\":1}}\n\n", i, m)
	}
	fl.Flush()
	var got []any
	if !f.stateless {
		for range f.send {
			select {
			case a := <-f.answerCh:
				got = append(got, a)
			case <-r.Context().Done():
				return
			case <-time.After(3 * time.Second):
				f.t.Error("server: no answer from the client")
			}
		}
	}
	b, _ := json.Marshal(got)
	resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
		"content": []any{map[string]any{"type": "text", "text": string(b)}},
	}})
	fmt.Fprintf(w, "event: message\ndata: %s\n\n", resp)
}

func (f *clientReqServer) answerByID(id string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.answers {
		if a["id"] == id {
			return a
		}
	}
	return nil
}

func (c *collector) serverReqs() []ServerRequestStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ServerRequestStats(nil), c.srvReqs...)
}

func TestSamplingAnsweredMidStream(t *testing.T) {
	f, srv := newClientReqServer(t, false, "ping", "sampling/createMessage")
	obs := &collector{}
	s := connect(t, srv.URL, "2025-11-25", obs, func(o *Options) {
		o.Sampling = &Responder{Result: json.RawMessage(`{"role":"assistant","content":{"type":"text","text":"ok"},"model":"m","stopReason":"endTurn"}`), Delay: 50 * time.Millisecond}
	})
	if _, ok := f.initCaps["sampling"]; !ok {
		t.Fatalf("sampling capability not declared: %v", f.initCaps)
	}
	if _, ok := f.initCaps["elicitation"]; ok {
		t.Fatalf("elicitation declared without a responder: %v", f.initCaps)
	}
	r := s.CallTool(context.Background(), "t", nil)
	if r.Err != nil {
		t.Fatalf("call failed: %v", r.Err)
	}
	if !strings.Contains(string(r.Content), `\"model\":\"m\"`) {
		t.Fatalf("final result does not carry the answer: %s", r.Content)
	}
	a := f.answerByID("srv-1")
	if a == nil || a["error"] != nil || a["result"].(map[string]any)["model"] != "m" {
		t.Fatalf("sampling answer: %v", a)
	}
	if p := f.answerByID("srv-0"); p == nil || p["error"] != nil {
		t.Fatalf("ping answer: %v", p)
	}
	for _, h := range f.hdrs {
		if h.Get(HeaderSessionID) != "s1" || h.Get(HeaderProtocolVersion) != "2025-11-25" {
			t.Fatalf("answer headers: %v", h)
		}
	}
	srs := obs.serverReqs()
	if len(srs) != 2 {
		t.Fatalf("server request events: %+v", srs)
	}
	for _, st := range srs {
		if st.ErrorType != "" || st.Status != http.StatusAccepted || st.Tool != "t" || st.NotAnswered {
			t.Fatalf("stats: %+v", st)
		}
		if st.Method == "sampling/createMessage" && st.Duration < 50*time.Millisecond {
			t.Fatalf("duration %v does not include the delay", st.Duration)
		}
	}
	if calls := obs.byMethod("tools/call"); len(calls) != 1 || calls[0].ErrorType != "" || calls[0].Duration < 50*time.Millisecond {
		t.Fatalf("tools/call stats: %+v", calls)
	}
}

func TestUnexpectedServerRequestGetsMethodNotFound(t *testing.T) {
	f, srv := newClientReqServer(t, false, "x/unknown")
	obs := &collector{}
	s := connect(t, srv.URL, "2025-11-25", obs, func(o *Options) { o.Sampling = &Responder{} })
	r := s.CallTool(context.Background(), "t", nil)
	if r.Err != nil {
		t.Fatalf("call failed: %v", r.Err)
	}
	a := f.answerByID("srv-0")
	e, _ := a["error"].(map[string]any)
	if e == nil || e["code"] != float64(CodeMethodNotFound) {
		t.Fatalf("answer: %v", a)
	}
	srs := obs.serverReqs()
	if len(srs) != 1 || srs[0].ErrorType != ErrUnsupportedRequest || srs[0].Method != "x/unknown" || srs[0].Status != 202 {
		t.Fatalf("stats: %+v", srs)
	}
}

func TestNoCapabilityConfigured(t *testing.T) {
	f, srv := newClientReqServer(t, false, "sampling/createMessage", "elicitation/create")
	obs := &collector{}
	s := connect(t, srv.URL, "2025-11-25", obs)
	for _, k := range []string{"sampling", "elicitation", "roots"} {
		if _, ok := f.initCaps[k]; ok {
			t.Fatalf("%s declared without a responder: %v", k, f.initCaps)
		}
	}
	r := s.CallTool(context.Background(), "t", nil)
	if r.Err != nil {
		t.Fatalf("call failed: %v", r.Err)
	}
	for _, id := range []string{"srv-0", "srv-1"} {
		e, _ := f.answerByID(id)["error"].(map[string]any)
		if e == nil || e["code"] != float64(CodeMethodNotFound) {
			t.Fatalf("%s: want -32601, got %v", id, f.answerByID(id))
		}
	}
	if srs := obs.serverReqs(); len(srs) != 2 || srs[0].ErrorType != ErrUnsupportedRequest || srs[1].ErrorType != ErrUnsupportedRequest {
		t.Fatalf("stats: %+v", srs)
	}
}

func TestResponderErrorAndElicitation(t *testing.T) {
	f, srv := newClientReqServer(t, false, "sampling/createMessage", "elicitation/create")
	obs := &collector{}
	s := connect(t, srv.URL, "2025-11-25", obs, func(o *Options) {
		o.Sampling = &Responder{Error: &ResponderError{Code: -1, Message: "User rejected sampling request"}}
		o.Elicitation = &Responder{Result: json.RawMessage(`{"action":"decline"}`)}
		o.Capabilities = map[string]any{"elicitation": map[string]any{"form": map[string]any{}, "url": map[string]any{}}}
	})
	if el, _ := f.initCaps["elicitation"].(map[string]any); el["url"] == nil {
		t.Fatalf("explicit capabilities entry overridden: %v", f.initCaps)
	}
	if r := s.CallTool(context.Background(), "t", nil); r.Err != nil {
		t.Fatalf("call failed: %v", r.Err)
	}
	if e, _ := f.answerByID("srv-0")["error"].(map[string]any); e == nil || e["code"] != float64(-1) {
		t.Fatalf("sampling answer: %v", f.answerByID("srv-0"))
	}
	if res, _ := f.answerByID("srv-1")["result"].(map[string]any); res["action"] != "decline" {
		t.Fatalf("elicitation answer: %v", f.answerByID("srv-1"))
	}
	// A configured error answer is the scripted behaviour, not a client error.
	for _, st := range obs.serverReqs() {
		if st.ErrorType != "" {
			t.Fatalf("stats: %+v", st)
		}
	}
}

// 2026-07-28 forbids server requests on response streams and client
// responses: nothing is POSTed, the request is counted, the call completes.
func TestStatelessServerRequestNotAnswered(t *testing.T) {
	f, srv := newClientReqServer(t, true, "sampling/createMessage")
	obs := &collector{}
	s := connect(t, srv.URL, ProtocolStateless, obs, func(o *Options) { o.Sampling = &Responder{} })
	if r := s.CallTool(context.Background(), "t", nil); r.Err != nil {
		t.Fatalf("call failed: %v", r.Err)
	}
	if len(f.answers) != 0 {
		t.Fatalf("client POSTed a response in stateless mode: %v", f.answers)
	}
	srs := obs.serverReqs()
	if len(srs) != 1 || !srs[0].NotAnswered || srs[0].ErrorType != ErrUnsupportedRequest || srs[0].Protocol != ProtocolStateless {
		t.Fatalf("stats: %+v", srs)
	}
}

// The delay is cut short when the caller's context ends.
func TestResponderDelayCancelled(t *testing.T) {
	_, srv := newClientReqServer(t, false, "sampling/createMessage")
	obs := &collector{}
	s := connect(t, srv.URL, "2025-11-25", obs, func(o *Options) { o.Sampling = &Responder{Delay: time.Hour} })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_ = s.CallTool(ctx, "t", nil)
	if time.Since(start) > 2*time.Second {
		t.Fatal("call waited for the full delay")
	}
	if srs := obs.serverReqs(); len(srs) != 1 || !srs[0].NotAnswered || srs[0].ErrorType != ErrTimeout {
		t.Fatalf("stats: %+v", srs)
	}
}
