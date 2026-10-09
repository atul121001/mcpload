package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/sampler"
)

// The test binary doubles as a fake engine: with fakeK6Env=1, TestMain runs
// fakeK6 instead of the tests.
const (
	fakeK6Env    = "MCPLOAD_TEST_FAKE_K6"
	fakeK6OutEnv = "MCPLOAD_TEST_FAKE_K6_OUT" // where `run` records what it saw
)

// fakeK6 answers `version`, `inspect` and `run` like k6 with xk6-mcpload
// would for a stdio run: `run` registers its own pid in MCPLOAD_PID_DIR as
// if it were a server process, lives for ~2.5 s and writes NDJSON with
// requests, stdout pollution and one unexpected process exit.
func fakeK6(args []string) int {
	if len(args) == 0 {
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Println("k6 v1.2.3 (fake)")
		return 0
	case "inspect":
		b, err := os.ReadFile(filepath.Join("..", "k6run", "testdata", "inspect-agent.json"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		os.Stdout.Write(b)
		return 0
	case "run":
	default:
		return 2
	}
	var ndjson, summary string
	for i := 1; i < len(args)-1; i++ {
		switch args[i] {
		case "--out":
			ndjson = strings.TrimPrefix(args[i+1], "json=")
		case "--summary-export":
			summary = args[i+1]
		}
	}
	seen, _ := json.Marshal(map[string]any{
		"args":       args,
		"commandEnv": os.Getenv("MCP_COMMAND_ENV"),
		"pidDir":     os.Getenv(sampler.PIDDirEnv),
	})
	if err := os.WriteFile(os.Getenv(fakeK6OutEnv), seen, 0o644); err != nil {
		return 1
	}
	pidFile := filepath.Join(os.Getenv(sampler.PIDDirEnv), strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(pidFile, nil, 0o644); err != nil {
		return 1
	}
	start := time.Now().UTC()
	var lines []string
	pt := func(at time.Duration, metric string, v float64, tags string) {
		lines = append(lines, fmt.Sprintf(`{"metric":%q,"type":"Point","data":{"time":%q,"value":%v,"tags":{"transport":"stdio"%s}}}`,
			metric, start.Add(at).Format(time.RFC3339Nano), v, tags))
	}
	pt(0, "mcp_process_spawn_duration", 25, "")
	pt(0, "mcp_processes_open", 1, "")
	for i := 0; i < 25; i++ {
		at := time.Duration(i) * 100 * time.Millisecond
		pt(at, "mcp_reqs", 1, `,"method":"tools/call","tool":"echo","protocol":"2025-11-25"`)
		pt(at, "mcp_req_duration", 3, `,"method":"tools/call","tool":"echo","protocol":"2025-11-25"`)
		pt(at, "iterations", 1, "")
	}
	pt(time.Second, "mcp_stdout_invalid_lines", 2, "")
	pt(2*time.Second, "mcp_process_exits", 1, `,"expected":"false","exit_code":"1"`)
	pt(2400*time.Millisecond, "mcp_process_exits", 1, `,"expected":"true","exit_code":"0"`)
	time.Sleep(2500 * time.Millisecond)
	os.Remove(pidFile)
	if err := os.WriteFile(ndjson, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return 1
	}
	os.WriteFile(summary, []byte(`{"metrics":{}}`), 0o644)
	return 0
}

func TestRunStdioEndToEnd(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	seenPath := filepath.Join(dir, "seen.json")
	out := filepath.Join(dir, "report.json")
	t.Setenv(fakeK6Env, "1")
	t.Setenv(fakeK6OutEnv, seenPath)
	t.Setenv("MCP_URL", "http://from-the-os-env/mcp")

	var stdout, stderr bytes.Buffer
	code := Main([]string{"run", "--engine", exe, "--scenario", "stdio_run_test.go", "--interval", "1s",
		"--command", `node "my server.mjs" --api-key s3cr3t-arg`, "--command-env", "API_TOKEN=s3cr3t-env",
		"--wait-ready", "1m", "--out", out}, &stdout, &stderr)
	t.Log(stderr.String())
	if code != ExitFail {
		t.Fatalf("exit %d, want %d (stdio verdicts fail)", code, ExitFail)
	}
	if strings.Contains(stderr.String(), "s3cr3t") {
		t.Error("a secret was printed")
	}
	for _, want := range []string{"--wait-ready: skipped for a stdio server", "target (stdio) node 'my server.mjs' --api-key REDACTED", "server environment adds API_TOKEN (values not shown)"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q", want)
		}
	}

	var seen struct {
		Args       []string
		CommandEnv string
		PIDDir     string
	}
	b, err := os.ReadFile(seenPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &seen); err != nil {
		t.Fatal(err)
	}
	if seen.CommandEnv != `{"API_TOKEN":"s3cr3t-env"}` {
		t.Errorf("MCP_COMMAND_ENV = %q", seen.CommandEnv)
	}
	args := strings.Join(seen.Args, " ")
	if strings.Contains(args, "s3cr3t-env") {
		t.Error("command env on the k6 command line")
	}
	if !strings.Contains(args, `MCP_COMMAND=["node","my server.mjs","--api-key","s3cr3t-arg"]`) || !strings.Contains(args, "-e MCP_URL= ") {
		t.Errorf("k6 args: %s", args)
	}
	if seen.PIDDir == "" {
		t.Fatal("no MCPLOAD_PID_DIR")
	}
	if _, err := os.Stat(seen.PIDDir); !os.IsNotExist(err) {
		t.Errorf("pid dir not removed: %v", err)
	}

	r, err := report.ReadJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Check(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(out)
	if bytes.Contains(raw, []byte("s3cr3t")) {
		t.Error("a secret is in report.json")
	}
	tg := r.Run.Target
	if tg.URL != "" || tg.Transport != report.TransportStdio || strings.Join(tg.Command, "|") != "node|my server.mjs|--api-key|REDACTED" {
		t.Errorf("target %+v", tg)
	}
	if r.Series.Server.Sampler != report.SamplerProcess {
		t.Errorf("sampler %q", r.Series.Server.Sampler)
	}
	haveRSS := false
	for _, v := range r.Series.Server.RSSBytes {
		haveRSS = haveRSS || (v != nil && *v > 0)
	}
	if !haveRSS {
		t.Errorf("no RSS sample of the fake server process: %v", r.Series.Server.RSSBytes)
	}
	got := map[string]report.Verdict{}
	for _, v := range r.Verdicts {
		got[v.ID] = v
	}
	if v := got[report.VerdictStdoutPollution]; v.Status != report.StatusFail || !strings.Contains(v.Message, "2 lines") {
		t.Errorf("stdout_pollution %+v", v)
	}
	if v := got[report.VerdictProcessExit]; v.Status != report.StatusFail || !strings.Contains(v.Message, "exit codes: 1 ×1") {
		t.Errorf("process_exit %+v", v)
	}
	if v := got[report.VerdictGenerator]; !strings.Contains(v.Message, "share its CPU") {
		t.Errorf("generator %+v", v)
	}
}
