package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// The test binary doubles as a fake stdio MCP server: with fakeStdioEnv set,
// TestMain runs fakeStdioServer instead of the tests.
const fakeStdioEnv = "MCPLOAD_FAKE_STDIO"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeStdioEnv); mode != "" {
		fakeStdioServer(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeStdioServer speaks newline-delimited JSON-RPC on stdin/stdout. mode is a
// comma-separated list of flags:
//
//	noise        write a log line to stdout before every response
//	ignore-eof   keep running after stdin closes (Close must kill it)
//	honor-cancel drop the response of a cancelled call
//
// Tools: echo (returns its args), slow {ms} (answers after ms, concurrently),
// crash (writes to stderr and exits 3), sample (asks the client for
// sampling/createMessage and returns the answer's text).
func fakeStdioServer(mode string) {
	flags := map[string]bool{}
	for _, f := range strings.Split(mode, ",") {
		flags[f] = true
	}
	if flags["sleeper"] {
		time.Sleep(time.Hour)
		return
	}
	if flags["launcher"] {
		// Like npx: start a child that inherits stdout and outlives us.
		exe, _ := os.Executable()
		child := exec.Command(exe)
		child.Env = append(os.Environ(), fakeStdioEnv+"=sleeper")
		child.Stdout = os.Stdout
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "launcher:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "child %d\n", child.Process.Pid)
	}
	var wmu sync.Mutex
	send := func(v any) {
		b, _ := json.Marshal(v)
		wmu.Lock()
		defer wmu.Unlock()
		if flags["noise"] {
			fmt.Fprintln(os.Stdout, "server: handling a request")
		}
		os.Stdout.Write(append(b, '\n'))
	}
	result := func(id json.RawMessage, r any) { send(map[string]any{"jsonrpc": "2.0", "id": id, "result": r}) }
	text := func(s string) map[string]any {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}
	}

	var mu sync.Mutex
	cancelled := map[string]bool{}
	waiting := map[string]chan json.RawMessage{} // server request id -> answer

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	for in.Scan() {
		var m rpcMessage
		if json.Unmarshal(in.Bytes(), &m) != nil {
			continue
		}
		if m.Method == "" { // a response to our sampling request
			mu.Lock()
			ch := waiting[string(m.ID)]
			mu.Unlock()
			if ch != nil {
				ch <- m.Result
			}
			continue
		}
		switch m.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(m.Params, &p)
			result(m.ID, map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "fake-stdio", "version": "1"}})
		case "notifications/initialized":
		case "notifications/cancelled":
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			_ = json.Unmarshal(m.Params, &p)
			mu.Lock()
			cancelled[string(p.RequestID)] = true
			mu.Unlock()
		case "tools/list":
			result(m.ID, map[string]any{"tools": []any{map[string]any{"name": "echo"}, map[string]any{"name": "slow"}}})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(m.Params, &p)
			id := m.ID
			switch p.Name {
			case "echo":
				result(id, text(string(p.Arguments)))
			case "slow":
				var a struct {
					MS int `json:"ms"`
				}
				_ = json.Unmarshal(p.Arguments, &a)
				go func() {
					time.Sleep(time.Duration(a.MS) * time.Millisecond)
					mu.Lock()
					drop := flags["honor-cancel"] && cancelled[string(id)]
					mu.Unlock()
					if !drop {
						result(id, text("slow done"))
					}
				}()
			case "crash":
				fmt.Fprintln(os.Stderr, "fatal: boom")
				os.Exit(3)
			case "sample":
				go func() {
					ch := make(chan json.RawMessage, 1)
					mu.Lock()
					waiting[`"s1"`] = ch
					mu.Unlock()
					send(map[string]any{"jsonrpc": "2.0", "id": "s1", "method": "sampling/createMessage", "params": map[string]any{}})
					var r struct {
						Content struct {
							Text string `json:"text"`
						} `json:"content"`
					}
					_ = json.Unmarshal(<-ch, &r)
					result(id, text("sampled: "+r.Content.Text))
				}()
			default:
				send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32602, "message": "unknown tool " + p.Name}})
			}
		default:
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "Method not found"}})
		}
	}
	if flags["ignore-eof"] {
		time.Sleep(time.Hour)
	}
}

// procCollector is a collector that also records process and cancel events.
type procCollector struct {
	collector
	pmu     sync.Mutex
	spawns  []ProcessStats
	exits   chan ProcessExitStats
	invalid []StdoutInvalidStats
	cancels chan CancelStats
}

func newProcCollector() *procCollector {
	return &procCollector{exits: make(chan ProcessExitStats, 4), cancels: make(chan CancelStats, 16)}
}

func (c *procCollector) OnProcessSpawn(s ProcessStats) {
	c.pmu.Lock()
	c.spawns = append(c.spawns, s)
	c.pmu.Unlock()
}
func (c *procCollector) OnProcessExit(s ProcessExitStats) { c.exits <- s }
func (c *procCollector) OnStdoutInvalid(s StdoutInvalidStats) {
	c.pmu.Lock()
	c.invalid = append(c.invalid, s)
	c.pmu.Unlock()
}
func (c *procCollector) OnCancel(s CancelStats) { c.cancels <- s }

func (c *procCollector) waitExit(t *testing.T) ProcessExitStats {
	t.Helper()
	select {
	case e := <-c.exits:
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("no process exit event")
		return ProcessExitStats{}
	}
}

func stdioOptions(t *testing.T, mode string, obs Observer) Options {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Options{Command: []string{exe}, Env: map[string]string{fakeStdioEnv: mode}, Observer: obs, Timeout: 5 * time.Second}
}

func connectStdio(t *testing.T, mode string, obs Observer) *Session {
	t.Helper()
	s, err := Connect(context.Background(), stdioOptions(t, mode, obs))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return s
}

func TestStdioHandshakeAndCalls(t *testing.T) {
	obs := newProcCollector()
	s := connectStdio(t, "plain", obs)
	if s.Protocol() != DefaultFallbackVersion || s.Stateless() || s.Transport() != TransportStdio || s.PID() == 0 {
		t.Fatalf("protocol %q stateless %v transport %q pid %d", s.Protocol(), s.Stateless(), s.Transport(), s.PID())
	}
	tools, err := s.ListTools(context.Background())
	if err != nil || len(tools) != 2 {
		t.Fatalf("tools %v err %v", tools, err)
	}
	r := s.CallTool(context.Background(), "echo", map[string]any{"x": 1})
	if r.Err != nil || !strings.Contains(firstText(r.Content), `"x":1`) {
		t.Fatalf("echo: %+v", r)
	}
	r = s.CallTool(context.Background(), "nope", nil)
	if r.Err == nil || r.Err.Type != ErrJSONRPC || r.Err.Code != -32602 {
		t.Fatalf("unknown tool: %+v", r.Err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := obs.waitExit(t)
	if !e.Expected || e.ExitCode != 0 {
		t.Fatalf("exit %+v", e)
	}

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.connects) != 1 || obs.connects[0].Transport != TransportStdio || obs.connects[0].Method != "initialize" ||
		obs.connects[0].Spawn <= 0 || obs.connects[0].ErrorType != "" {
		t.Fatalf("connects %+v", obs.connects)
	}
	for _, r := range obs.reqs {
		if r.Transport != TransportStdio || r.Status != 0 {
			t.Fatalf("request stats %+v", r)
		}
	}
	if len(obs.spawns) != 1 || obs.spawns[0].PID != s.PID() {
		t.Fatalf("spawns %+v", obs.spawns)
	}
}

// Calls on one process run concurrently: four 300 ms calls take ~300 ms.
func TestStdioParallelCalls(t *testing.T) {
	s := connectStdio(t, "plain", NopObserver{})
	defer s.Close(context.Background())
	calls := make([]ToolCall, 4)
	for i := range calls {
		calls[i] = ToolCall{Name: "slow", Args: map[string]any{"ms": 300}}
	}
	start := time.Now()
	for i, r := range s.CallParallel(context.Background(), calls) {
		if r.Err != nil {
			t.Fatalf("call %d: %v", i, r.Err)
		}
	}
	if d := time.Since(start); d > 1100*time.Millisecond {
		t.Fatalf("parallel calls took %s", d)
	}
}

func TestStdioCrashMidCall(t *testing.T) {
	obs := newProcCollector()
	s := connectStdio(t, "plain", obs)
	defer s.Close(context.Background())
	slow := make(chan ToolResult, 1)
	go func() { slow <- s.CallTool(context.Background(), "slow", map[string]any{"ms": 3000}) }()
	time.Sleep(100 * time.Millisecond)
	r := s.CallTool(context.Background(), "crash", nil)
	for _, r := range []ToolResult{r, <-slow} {
		if r.Err == nil || r.Err.Type != ErrProcessExit || !strings.Contains(r.Err.Message, "fatal: boom") {
			t.Fatalf("want process_exit with the stderr tail, got %+v", r.Err)
		}
	}
	e := obs.waitExit(t)
	if e.Expected || e.ExitCode != 3 || e.StderrTail != "fatal: boom" {
		t.Fatalf("exit %+v", e)
	}
	// Later requests fail at once.
	if r := s.CallTool(context.Background(), "echo", nil); r.Err == nil || r.Err.Type != ErrProcessExit {
		t.Fatalf("after exit: %+v", r.Err)
	}
}

func TestStdioStdoutNoise(t *testing.T) {
	obs := newProcCollector()
	s := connectStdio(t, "noise", obs)
	defer s.Close(context.Background())
	if r := s.CallTool(context.Background(), "echo", nil); r.Err != nil {
		t.Fatal(r.Err)
	}
	if n := s.StdoutInvalidLines(); n < 2 { // initialize + the call
		t.Fatalf("invalid lines %d", n)
	}
	obs.pmu.Lock()
	defer obs.pmu.Unlock()
	if len(obs.invalid) == 0 || obs.invalid[0].Sample != "server: handling a request" {
		t.Fatalf("invalid %+v", obs.invalid)
	}
}

func TestStdioServerRequest(t *testing.T) {
	obs := newProcCollector()
	o := stdioOptions(t, "plain", obs)
	o.Sampling = &Responder{Result: json.RawMessage(`{"role":"assistant","content":{"type":"text","text":"hi"},"model":"m"}`)}
	s, err := Connect(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	r := s.CallTool(context.Background(), "sample", nil)
	if r.Err != nil || firstText(r.Content) != "sampled: hi" {
		t.Fatalf("sample: %+v %s", r.Err, r.Content)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.srvReqs) != 1 || obs.srvReqs[0].Method != "sampling/createMessage" || obs.srvReqs[0].Tool != "sample" || obs.srvReqs[0].ErrorType != "" {
		t.Fatalf("server requests %+v", obs.srvReqs)
	}
}

func TestStdioCancel(t *testing.T) {
	for _, tc := range []struct{ mode, want string }{
		{"plain", CancelOutcomeLateResponse},
		{"honor-cancel", CancelOutcomeCancelled},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			obs := newProcCollector()
			o := stdioOptions(t, tc.mode, obs)
			o.CancelWait = time.Second
			s, err := Connect(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background())
			r := s.CallToolWith(context.Background(), "slow", map[string]any{"ms": 300}, CallOptions{CancelAfter: 50 * time.Millisecond})
			if !r.Cancelled {
				t.Fatalf("not cancelled: %+v", r)
			}
			select {
			case cs := <-obs.cancels:
				if cs.Outcome != tc.want {
					t.Fatalf("outcome %q, want %q", cs.Outcome, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no cancel event")
			}
			if len(obs.byMethod("notifications/cancelled")) != 1 {
				t.Fatal("notifications/cancelled not sent")
			}
		})
	}
}

func TestStdioTimeout(t *testing.T) {
	o := stdioOptions(t, "plain", NopObserver{})
	o.Timeout = 200 * time.Millisecond
	s, err := Connect(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if r := s.CallTool(context.Background(), "slow", map[string]any{"ms": 1000}); r.Err == nil || r.Err.Type != ErrTimeout {
		t.Fatalf("want timeout, got %+v", r.Err)
	}
	// The late response is dropped and the session keeps working.
	time.Sleep(900 * time.Millisecond)
	if r := s.CallTool(context.Background(), "echo", nil); r.Err != nil {
		t.Fatal(r.Err)
	}
}

// A server that ignores stdin closing is killed after the grace period.
func TestStdioCloseKillsStubbornServer(t *testing.T) {
	obs := newProcCollector()
	s := connectStdio(t, "ignore-eof", obs)
	start := time.Now()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < stdioCloseGrace || d > stdioCloseGrace+5*time.Second {
		t.Fatalf("close took %s", d)
	}
	if e := obs.waitExit(t); !e.Expected || e.ExitCode == 0 {
		t.Fatalf("exit %+v", e)
	}
}

func TestStdioSpawnFailure(t *testing.T) {
	obs := newProcCollector()
	_, err := Connect(context.Background(), Options{Command: []string{"mcpload-no-such-program-xyz"}, Observer: obs})
	if e := AsError(err); e == nil || e.Type != ErrProcessSpawn {
		t.Fatalf("want process_spawn, got %v", err)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.connects) != 1 || obs.connects[0].ErrorType != ErrProcessSpawn || obs.open != 0 {
		t.Fatalf("connects %+v open %d", obs.connects, obs.open)
	}
}

func TestReadLineTooLong(t *testing.T) {
	in := strings.Repeat("x", 100) + "\n" + "ok\r\n"
	br := bufio.NewReaderSize(strings.NewReader(in), 16)
	if line, tooLong, _ := readLine(br, 50); !tooLong || line != nil {
		t.Fatalf("got %q tooLong=%v", line, tooLong)
	}
	if line, tooLong, _ := readLine(br, 50); tooLong || string(line) != "ok" {
		t.Fatalf("got %q tooLong=%v", line, tooLong)
	}
}

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 10}
	fmt.Fprint(b, "line one\nline two\n")
	if got := b.tail(10); got != "line two" {
		t.Fatalf("tail %q", got)
	}
}

// A launcher (npx, uvx, a shell script) leaves its child running when it
// exits; Close must take the whole tree down.
func TestStdioCloseKillsChildren(t *testing.T) {
	obs := newProcCollector()
	s := connectStdio(t, "launcher", obs)
	r := s.CallTool(context.Background(), "echo", nil)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	var child int
	if _, err := fmt.Sscanf(s.stdio.stderr.tail(100), "child %d", &child); err != nil || child == 0 {
		t.Fatalf("no child pid on stderr: %q", s.stdio.stderr.tail(100))
	}
	if !processAlive(child) {
		t.Fatal("child not running before Close")
	}
	_ = s.Close(context.Background())
	obs.waitExit(t)
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(child) {
		if time.Now().After(deadline) {
			t.Fatalf("child %d still running after Close", child)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
