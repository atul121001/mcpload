package mcpload

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atul121001/mcpload/xk6-mcpload/client"

	"go.k6.io/k6/v2/js/modulestest"
	"go.k6.io/k6/v2/lib"
	"go.k6.io/k6/v2/metrics"
)

// miniServer is a minimal stateful MCP server (SSE responses).
func miniServer(t *testing.T) *httptest.Server {
	var mu sync.Mutex
	sessions := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			delete(sessions, r.Header.Get("Mcp-Session-Id"))
			mu.Unlock()
			return
		}
		b, _ := io.ReadAll(r.Body)
		var m struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.Unmarshal(b, &m)
		var result any
		switch m.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			mu.Lock()
			sessions["s1"] = true
			mu.Unlock()
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
		case "notifications/initialized":
			w.WriteHeader(202)
			return
		case "server/discover":
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"Bad Request: not initialized"}}`))
			return
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			if m.Params["name"] == "fail" {
				result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "nope"}}}
			} else {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}
			}
		case "ping":
			result = map[string]any{}
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message\ndata: " + string(out) + "\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestJSAPI(t *testing.T) {
	srv := miniServer(t)
	rt := modulestest.NewRuntime(t)
	registry := rt.VU.InitEnvField.Registry
	m, ok := New().NewModuleInstance(rt.VU).(*ModuleInstance)
	if !ok {
		t.Fatal("bad instance")
	}
	if err := rt.VU.Runtime().Set("mcp", m.Exports().Named); err != nil {
		t.Fatal(err)
	}
	// Init context: construct the client.
	if _, err := rt.RunOnEventLoop(`var client = new mcp.Client({url: "` + srv.URL + `", protocol: "auto", timeout: "5s", headers: {"X-Test": "1"}});`); err != nil {
		t.Fatal(err)
	}

	samples := make(chan metrics.SampleContainer, 1000)
	rt.MoveToVUContext(&lib.State{
		Samples:   samples,
		Tags:      lib.NewVUStateTags(registry.RootTagSet().With("scenario", "default")),
		Transport: http.DefaultTransport,
	})

	v, err := rt.RunOnEventLoop(`
		const s = client.connect();
		const tools = s.listTools();
		const r = s.callTool("echo", {q: 1});
		const bad = s.callTool("fail");
		const par = s.callParallel([{name: "echo", args: {}}, {name: "fail"}, {name: "echo"}]);
		s.ping();
		s.close();
		JSON.stringify({proto: s.protocol, sid: s.sessionId, n: tools.length, t0: tools[0].name,
			ok: r.isError, text: r.content[0].text, dur: typeof r.durationMs,
			bad: bad.error.type, par: par.map(x => x.isError), isArr: Array.isArray(par)});
	`)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatal(err)
	}
	want := `{"bad":"tool_iserror","dur":"number","isArr":true,"n":1,"ok":false,"par":[false,true,false],"proto":"2025-06-18","sid":"s1","t0":"echo","text":"ok"}`
	if b, _ := json.Marshal(got); string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}

	close(samples)
	seen := map[string]int{}
	var toolErr []float64
	for sc := range samples {
		for _, s := range sc.GetSamples() {
			seen[s.Metric.Name]++
			if s.Metric.Name == "mcp_tool_error_rate" {
				toolErr = append(toolErr, s.Value)
			}
			if s.Metric.Name == "mcp_req_duration" {
				if sc, _ := s.Tags.Get("scenario"); sc != "default" {
					t.Errorf("VU tags not propagated: %v", s.Tags.Map())
				}
			}
		}
	}
	for _, name := range []string{"mcp_req_duration", "mcp_req_ttfb", "mcp_stream_duration", "mcp_connect_duration", "mcp_reqs", "mcp_errors", "mcp_tool_error_rate", "mcp_sessions_open"} {
		if seen[name] == 0 {
			t.Errorf("no samples for %s (%v)", name, seen)
		}
	}
	if len(toolErr) != 5 {
		t.Errorf("tool error rate samples: %v", toolErr)
	}
	// errors: 2 tool_iserror (the auto probe answered 400 is not an error)
	if seen["mcp_errors"] != 2 {
		t.Errorf("mcp_errors = %d", seen["mcp_errors"])
	}
}

func TestConnectErrorThrowsWithType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	rt := modulestest.NewRuntime(t)
	m := New().NewModuleInstance(rt.VU).(*ModuleInstance)
	_ = rt.VU.Runtime().Set("mcp", m.Exports().Named)
	if _, err := rt.RunOnEventLoop(`var client = new mcp.Client({url: "` + srv.URL + `", protocol: "2025-06-18", auth: {type: "bearer", token: "x"}});`); err != nil {
		t.Fatal(err)
	}
	rt.MoveToVUContext(&lib.State{
		Samples:   make(chan metrics.SampleContainer, 100),
		Tags:      lib.NewVUStateTags(metrics.NewRegistry().RootTagSet()),
		Transport: http.DefaultTransport,
	})
	v, err := rt.RunOnEventLoop(`let t; try { client.connect(); } catch (e) { t = e.type + ":" + e.status + ":" + (e instanceof Error); } t`)
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != "auth:401:true" {
		t.Fatalf("got %s", v)
	}
}

func TestBadOptions(t *testing.T) {
	rt := modulestest.NewRuntime(t)
	m := New().NewModuleInstance(rt.VU).(*ModuleInstance)
	_ = rt.VU.Runtime().Set("mcp", m.Exports().Named)
	for _, js := range []string{`new mcp.Client({})`, `new mcp.Client({url: "x", bogus: 1})`, `new mcp.Client({url: "x", auth: {type: "oauth"}})`} {
		if _, err := rt.RunOnEventLoop(js); err == nil || !strings.Contains(err.Error(), "mcp.Client") {
			t.Errorf("%s: expected error, got %v", js, err)
		}
	}
	// connect in init context must throw
	if _, err := rt.RunOnEventLoop(`new mcp.Client({url: "http://127.0.0.1:1"}).connect()`); err == nil || !strings.Contains(err.Error(), "VU context") {
		t.Errorf("expected init-context error, got %v", err)
	}
}

// Two VUs (module instances) with their own Client for the same URL share
// the process-wide protocol cache: only the first connect probes.
func TestRememberProtocolAcrossVUs(t *testing.T) {
	inner := miniServer(t)
	var mu sync.Mutex
	probes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"server/discover"`) {
			mu.Lock()
			probes++
			mu.Unlock()
		}
		req, _ := http.NewRequest(r.Method, inner.URL, strings.NewReader(string(b)))
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)

	run := func(remember bool) {
		rt := modulestest.NewRuntime(t)
		m := New().NewModuleInstance(rt.VU).(*ModuleInstance)
		_ = rt.VU.Runtime().Set("mcp", m.Exports().Named)
		opt := ""
		if !remember {
			opt = ", rememberProtocol: false"
		}
		if _, err := rt.RunOnEventLoop(`var client = new mcp.Client({url: "` + srv.URL + `"` + opt + `});`); err != nil {
			t.Fatal(err)
		}
		rt.MoveToVUContext(&lib.State{
			Samples:   make(chan metrics.SampleContainer, 1000),
			Tags:      lib.NewVUStateTags(metrics.NewRegistry().RootTagSet()),
			Transport: http.DefaultTransport,
		})
		v, err := rt.RunOnEventLoop(`const s = client.connect(); s.close(); s.protocol`)
		if err != nil {
			t.Fatal(err)
		}
		if v.String() != "2025-06-18" {
			t.Fatalf("protocol %s", v)
		}
	}
	run(true)
	run(true)
	if probes != 1 {
		t.Fatalf("two VUs probed %d times, want 1", probes)
	}
	run(false)
	if probes != 2 {
		t.Fatalf("rememberProtocol:false should probe, got %d probes", probes)
	}
}

func TestParseAuthFailureBackoff(t *testing.T) {
	cases := map[string]time.Duration{"": 0, "'250ms'": 250 * time.Millisecond, "2000": 2 * time.Second, "'0s'": -1, "0": -1}
	for js, want := range cases {
		rt := modulestest.NewRuntime(t)
		v := `{type: "oauth", tokenUrl: "http://x", clientId: "a"` + map[bool]string{true: "", false: ", failureBackoff: " + js}[js == ""] + `}`
		val, err := rt.VU.Runtime().RunString("(" + v + ")")
		if err != nil {
			t.Fatal(err)
		}
		a, err := parseAuth(val.Export())
		if err != nil {
			t.Fatalf("%s: %v", js, err)
		}
		ts := a.(*client.TokenSource)
		if got := ts.Config().FailureBackoff; got != want {
			t.Errorf("%s: FailureBackoff = %v, want %v", js, got, want)
		}
	}
	rt := modulestest.NewRuntime(t)
	val, _ := rt.VU.Runtime().RunString(`({type: "oauth", tokenUrl: "http://x", clientId: "a", failureBackoff: "soon"})`)
	if _, err := parseAuth(val.Export()); err == nil {
		t.Error("expected error for bad failureBackoff")
	}
}

// servedBy on the session, tool results and thrown errors comes from the
// response header named by the servedByHeader option.
func TestJSServedBy(t *testing.T) {
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		k := n
		mu.Unlock()
		w.Header().Set("X-Replica", map[bool]string{true: "a", false: "b"}[k%2 == 1])
		b, _ := io.ReadAll(r.Body)
		var m struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(b, &m)
		var result any
		switch {
		case m.Method == "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
		case m.Method == "notifications/initialized":
			w.WriteHeader(202)
			return
		case k > 4: // the other replica: it doesn't know the session
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"Session not found"}}`))
			return
		default:
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	rt := modulestest.NewRuntime(t)
	m := New().NewModuleInstance(rt.VU).(*ModuleInstance)
	_ = rt.VU.Runtime().Set("mcp", m.Exports().Named)
	if _, err := rt.RunOnEventLoop(`var client = new mcp.Client({url: "` + srv.URL + `", protocol: "2025-06-18", servedByHeader: "X-Replica", timeout: "5s"});`); err != nil {
		t.Fatal(err)
	}
	rt.MoveToVUContext(&lib.State{
		Samples:   make(chan metrics.SampleContainer, 1000),
		Tags:      lib.NewVUStateTags(metrics.NewRegistry().RootTagSet()),
		Transport: http.DefaultTransport,
	})
	// requests: 1 initialize (a), 2 notifications/initialized (b), 3 call (a), 4 call (b), 5 call -> 404 (a), 6 ping -> 404 (b)
	v, err := rt.RunOnEventLoop(`
		const s = client.connect();
		const r = [s.callTool("echo"), s.callTool("echo"), s.callTool("echo")];
		let pe; try { s.ping(); } catch (e) { pe = e.type + ":" + e.servedBy; }
		JSON.stringify([s.servedBy, r[0].servedBy, r[1].servedBy, r[2].servedBy, r[2].error.type, r[2].error.servedBy, pe]);
	`)
	if err != nil {
		t.Fatal(err)
	}
	if want := `["a","a","b","a","session_not_found","a","session_not_found:b"]`; v.String() != want {
		t.Fatalf("got  %s\nwant %s", v, want)
	}
}
