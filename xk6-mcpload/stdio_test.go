package mcpload

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.k6.io/k6/v2/js/modulestest"
	"go.k6.io/k6/v2/lib"
	"go.k6.io/k6/v2/metrics"

	"github.com/atul121001/mcpload/xk6-mcpload/client"
)

// The test binary doubles as a fake stdio MCP server: with fakeStdioEnv set,
// TestMain runs fakeStdioServer instead of the tests.
const fakeStdioEnv = "MCPLOAD_EXT_FAKE_STDIO"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeStdioEnv); mode != "" {
		fakeStdioServer(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeStdioServer is a minimal stdio MCP server. Tools: echo (returns "ok"),
// noisy (writes a log line to stdout, then answers), crash (exits 3).
func fakeStdioServer(string) {
	out := bufio.NewWriter(os.Stdout)
	send := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = out.Write(append(b, '\n'))
		_ = out.Flush()
	}
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name            string `json:"name"`
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &m) != nil {
			continue
		}
		result := func(r any) { send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": r}) }
		switch m.Method {
		case "initialize":
			result(map[string]any{"protocolVersion": m.Params.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "fake", "version": "1"}})
		case "tools/call":
			switch m.Params.Name {
			case "crash":
				fmt.Fprintln(os.Stderr, "fatal: boom")
				os.Exit(3)
			case "noisy":
				_, _ = out.WriteString("server: handling a request\n")
			}
			result(map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}})
		case "ping":
			result(map[string]any{})
		}
	}
}

func jsString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStdioOptionValidation(t *testing.T) {
	cases := []struct {
		opts    string
		wantErr string // "" = accepted
		wantCmd []string
	}{
		{`{command: "node", args: ["server.mjs", "--x"], env: {K: "V", N: 1}, cwd: "."}`, "", []string{"node", "server.mjs", "--x"}},
		{`{command: ["npx", "-y", "srv"]}`, "", []string{"npx", "-y", "srv"}},
		{`{command: ["uvx"], args: ["srv"]}`, "", []string{"uvx", "srv"}},
		{`{command: "node", url: "http://x"}`, "mutually exclusive", nil},
		{`{command: ""}`, "command must not be empty", nil},
		{`{command: []}`, "non-empty program", nil},
		{`{command: 5}`, "command must be a string or an array of strings, got a number", nil},
		{`{command: ["node", 1]}`, "element 1 is a number", nil},
		{`{command: "node", args: "server.mjs"}`, "args must be an array of strings", nil},
		{`{command: "node", args: [true]}`, "args must be an array of strings", nil},
		{`{command: "node", env: "K=V"}`, "env must be an object", nil},
		{`{command: "node", env: {K: {a: 1}}}`, "env.K must be a string", nil},
		{`{command: "node", env: {"A=B": "x"}}`, "invalid variable name", nil},
		{`{command: "node", cwd: 1}`, "cwd must be a string", nil},
		{`{command: "node", headers: {A: "b"}}`, "headers is an HTTP option", nil},
		{`{command: "node", auth: {type: "bearer", token: "x"}}`, "auth is an HTTP option", nil},
		{`{url: "http://x", args: ["a"]}`, "args needs command", nil},
		{`{url: "http://x", env: {}}`, "env needs command", nil},
		{`{}`, "url (streamable HTTP) or command (stdio) is required", nil},
	}
	for _, tc := range cases {
		t.Run(tc.opts, func(t *testing.T) {
			rt := modulestest.NewRuntime(t)
			v, err := rt.VU.Runtime().RunString(`(` + tc.opts + `)`)
			if err != nil {
				t.Fatal(err)
			}
			c := &jsClient{}
			err = c.parseOptions(rt.VU.Runtime(), v)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
			if tc.wantCmd != nil && strings.Join(c.opts.Command, "|") != strings.Join(tc.wantCmd, "|") {
				t.Fatalf("command %q want %q", c.opts.Command, tc.wantCmd)
			}
		})
	}
	// Through the constructor: the error is a JS error naming mcp.Client.
	rt := modulestest.NewRuntime(t)
	m := New().NewModuleInstance(rt.VU).(*ModuleInstance)
	_ = rt.VU.Runtime().Set("mcp", m.Exports().Named)
	if _, err := rt.RunOnEventLoop(`new mcp.Client({command: "node", url: "http://x"})`); err == nil || !strings.Contains(err.Error(), "mcp.Client") {
		t.Fatalf("got %v", err)
	}
	v, err := rt.RunOnEventLoop(`const c = new mcp.Client({command: "node", args: ["s.mjs"], env: {K: "V"}}); c.transport + ":" + c.command.join(" ")`)
	if err != nil || v.String() != "stdio:node s.mjs" {
		t.Fatalf("got %v, %v", v, err)
	}
}

// drain collects samples until cond holds for them (or the deadline).
func drain(t *testing.T, ch chan metrics.SampleContainer, cond func([]metrics.Sample) bool) []metrics.Sample {
	t.Helper()
	var ss []metrics.Sample
	deadline := time.After(10 * time.Second)
	for !cond(ss) {
		select {
		case sc := <-ch:
			ss = append(ss, sc.GetSamples()...)
		case <-deadline:
			t.Fatalf("timed out waiting for samples; have %d", len(ss))
		}
	}
	// Pick up anything already queued.
	for {
		select {
		case sc := <-ch:
			ss = append(ss, sc.GetSamples()...)
		default:
			return ss
		}
	}
}

func countWhere(ss []metrics.Sample, name string, tags map[string]string) int {
	n := 0
next:
	for _, s := range ss {
		if s.Metric.Name != name {
			continue
		}
		for k, v := range tags {
			if got, _ := s.Tags.Get(k); got != v {
				continue next
			}
		}
		n++
	}
	return n
}

func TestStdioJSAPIAndMetrics(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidDir := t.TempDir()
	t.Setenv(pidDirEnv, pidDir)
	baseProcs := currentOpenProcesses()

	rt := modulestest.NewRuntime(t)
	registry := rt.VU.InitEnvField.Registry
	m := New().NewModuleInstance(rt.VU).(*ModuleInstance)
	_ = rt.VU.Runtime().Set("mcp", m.Exports().Named)
	if _, err := rt.RunOnEventLoop(`var client = new mcp.Client({command: [` + jsString(t, exe) + `], env: {` + fakeStdioEnv + `: "1"}, timeout: "5s"});`); err != nil {
		t.Fatal(err)
	}
	samples := make(chan metrics.SampleContainer, 1000)
	rt.MoveToVUContext(&lib.State{
		Samples: samples,
		Tags:    lib.NewVUStateTags(registry.RootTagSet().With("scenario", "default")),
	})

	v, err := rt.RunOnEventLoop(`
		var s = client.connect();
		const r = s.callTool("echo", {});
		const n = s.callTool("noisy", {});
		s.ping();
		JSON.stringify({t: s.transport, pid: typeof s.pid, text: r.content[0].text, noisy: n.isError, proto: s.protocol});
	`)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"t":"stdio","pid":"number","text":"ok","noisy":false,"proto":"2025-11-25"}`; v.String() != want {
		t.Fatalf("got  %s\nwant %s", v, want)
	}
	pidV, _ := rt.RunOnEventLoop(`s.pid`)
	pid := int(pidV.ToInteger())
	b, err := os.ReadFile(filepath.Join(pidDir, strconv.Itoa(pid)))
	if err != nil || string(b) != strconv.Itoa(pid)+"\n" {
		t.Fatalf("pid file: %q %v", b, err)
	}
	if got := currentOpenProcesses(); got != baseProcs+1 {
		t.Fatalf("open processes %d want %d", got, baseProcs+1)
	}

	// close: expected exit, PID file removed.
	if _, err := rt.RunOnEventLoop(`s.close()`); err != nil {
		t.Fatal(err)
	}
	ss := drain(t, samples, func(ss []metrics.Sample) bool { return countWhere(ss, "mcp_process_exits", nil) == 1 })
	if _, err := os.Stat(filepath.Join(pidDir, strconv.Itoa(pid))); !os.IsNotExist(err) {
		t.Fatalf("pid file not removed: %v", err)
	}
	if got := currentOpenProcesses(); got != baseProcs {
		t.Fatalf("open processes %d want %d", got, baseProcs)
	}

	// A crash mid-call: process_exit error, unexpected exit with code 3.
	v, err = rt.RunOnEventLoop(`
		var s2 = client.connect();
		const c = s2.callTool("crash", {});
		c.error.type;
	`)
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != client.ErrProcessExit {
		t.Fatalf("crash error type %s", v)
	}
	ss = append(ss, drain(t, samples, func(ss []metrics.Sample) bool { return countWhere(ss, "mcp_process_exits", nil) == 1 })...)
	if _, err := rt.RunOnEventLoop(`s2.close()`); err != nil { // after the exit: it stays unexpected
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(pidDir); len(entries) != 0 {
		t.Fatalf("pid dir not empty: %v", entries)
	}

	for _, s := range ss {
		switch s.Metric.Name {
		case "mcp_sessions_open", "mcp_processes_open":
			continue // process-wide gauges: test-wide tags only
		}
		if tr, _ := s.Tags.Get("transport"); tr != "stdio" {
			t.Errorf("%s: transport=%q (%v)", s.Metric.Name, tr, s.Tags.Map())
		}
		if st, has := s.Tags.Get("status"); has {
			t.Errorf("%s: stdio sample has status=%q", s.Metric.Name, st)
		}
		if s.Metric.Name == "mcp_req_ttfb" {
			t.Errorf("stdio request has a TTFB sample")
		}
	}
	checks := []struct {
		name string
		tags map[string]string
		want int
	}{
		{"mcp_process_spawn_duration", nil, 2},
		{"mcp_connect_duration", map[string]string{"method": "initialize"}, 2},
		{"mcp_reqs", map[string]string{"method": "tools/call"}, 3},
		{"mcp_reqs", map[string]string{"method": "tools/call", "error_type": client.ErrProcessExit}, 1},
		{"mcp_errors", map[string]string{"error_type": client.ErrProcessExit}, 1},
		{"mcp_stdout_invalid_lines", nil, 1},
		{"mcp_process_exits", map[string]string{"expected": "true", "exit_code": "0"}, 1},
		{"mcp_process_exits", map[string]string{"expected": "false", "exit_code": "3"}, 1},
		{"mcp_processes_open", nil, 4},
	}
	for _, c := range checks {
		if got := countWhere(ss, c.name, c.tags); got != c.want {
			t.Errorf("%s %v: %d samples, want %d", c.name, c.tags, got, c.want)
		}
	}
}

func TestStdioSpawnFailure(t *testing.T) {
	rt := modulestest.NewRuntime(t)
	registry := rt.VU.InitEnvField.Registry
	m := New().NewModuleInstance(rt.VU).(*ModuleInstance)
	_ = rt.VU.Runtime().Set("mcp", m.Exports().Named)
	if _, err := rt.RunOnEventLoop(`var client = new mcp.Client({command: "mcpload-no-such-program-xyz"});`); err != nil {
		t.Fatal(err)
	}
	samples := make(chan metrics.SampleContainer, 100)
	rt.MoveToVUContext(&lib.State{Samples: samples, Tags: lib.NewVUStateTags(registry.RootTagSet())})
	v, err := rt.RunOnEventLoop(`let t; try { client.connect(); } catch (e) { t = e.type; } t`)
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != client.ErrProcessSpawn {
		t.Fatalf("got %s", v)
	}
	close(samples)
	var ss []metrics.Sample
	for sc := range samples {
		ss = append(ss, sc.GetSamples()...)
	}
	tags := map[string]string{"transport": "stdio", "error_type": client.ErrProcessSpawn}
	if countWhere(ss, "mcp_process_spawn_duration", tags) != 1 || countWhere(ss, "mcp_connect_duration", tags) != 1 {
		t.Fatalf("samples: %v", ss)
	}
	if countWhere(ss, "mcp_processes_open", nil) != 0 {
		t.Fatal("a failed spawn changed mcp_processes_open")
	}
}

// HTTP samples carry transport=http and keep their status tag.
func TestTransportTagHTTP(t *testing.T) {
	r := metrics.NewRegistry()
	m, err := registerMetrics(r)
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan metrics.SampleContainer, 10)
	e := newTestEmitter(t, context.Background(), r, m, ch, "a", nil)
	now := time.Now()
	e.OnRequest(client.RequestStats{Method: "tools/call", Status: 200, Start: now, Duration: time.Millisecond})
	e.OnConnect(client.ConnectStats{Method: "initialize", Status: 200, Start: now})
	e.OnCancel(client.CancelStats{Method: "tools/call", Outcome: client.CancelOutcomeCancelled, Status: 202, Start: now})
	e.OnServerRequest(client.ServerRequestStats{Method: "roots/list", Status: 202, Start: now})
	e.OnTokenFetch(client.TokenStats{Status: 200, Start: now})
	close(ch)
	n := 0
	for sc := range ch {
		for _, s := range sc.GetSamples() {
			n++
			if tr, _ := s.Tags.Get("transport"); tr != "http" {
				t.Errorf("%s: transport=%q", s.Metric.Name, tr)
			}
			if _, has := s.Tags.Get("status"); !has {
				t.Errorf("%s: no status tag", s.Metric.Name)
			}
		}
	}
	if n == 0 {
		t.Fatal("no samples")
	}
}

// Events from background goroutines after the iteration (or the test) ended
// must neither block nor panic.
func TestPushSafeAfterEnd(t *testing.T) {
	sc := metrics.ConnectedSamples{}
	closed := make(chan metrics.SampleContainer, 1)
	close(closed)
	if pushSafe(context.Background(), closed, sc) {
		t.Fatal("push to a closed channel reported success")
	}
	full := make(chan metrics.SampleContainer) // unbuffered, nobody reads
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool)
	go func() { done <- pushSafe(ctx, full, sc) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("blocked push reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pushSafe still blocked after the context ended")
	}
	// A late exit after the context ended still updates the gauge counter.
	r := metrics.NewRegistry()
	m, _ := registerMetrics(r)
	base := currentOpenProcesses()
	e := newTestEmitter(t, ctx, r, m, closed, "a", nil)
	e.OnProcessSpawn(client.ProcessStats{PID: 1 << 30, Start: time.Now()})
	e.OnProcessExit(client.ProcessExitStats{PID: 1 << 30, ExitCode: -1})
	e.OnStdoutInvalid(client.StdoutInvalidStats{})
	if currentOpenProcesses() != base {
		t.Fatalf("counter %d want %d", currentOpenProcesses(), base)
	}
}
