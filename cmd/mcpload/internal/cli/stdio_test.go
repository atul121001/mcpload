package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/k6run"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/sampler"
)

// parseRun parses run flags like runCmd and validates them.
func parseRun(t *testing.T, args ...string) (*runOpts, error) {
	t.Helper()
	o := &runOpts{set: map[string]bool{}}
	fs := runFlags(o, io.Discard)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	fs.Visit(func(f *flag.Flag) { o.set[f.Name] = true })
	return o, o.validate(nil)
}

func TestTransportFlags(t *testing.T) {
	const scen = "stdio_test.go" // any existing file works as a script path
	cases := []struct {
		name  string
		args  []string
		err   string
		stdio bool
	}{
		{name: "neither", args: nil, err: "--url (an MCP endpoint) or --command (a local stdio server) is required"},
		{name: "both", args: []string{"--url", "http://h/mcp", "--command", "node s.js"}, err: "mutually exclusive"},
		{name: "url", args: []string{"--url", "http://h/mcp"}},
		{name: "command", args: []string{"--command", "node s.js"}, stdio: true},
		{name: "explicit stdio", args: []string{"--command", "node s.js", "--transport", "stdio"}, stdio: true},
		{name: "stdio without command", args: []string{"--url", "http://h/mcp", "--transport", "stdio"}, err: "--transport stdio needs --command"},
		{name: "http with command", args: []string{"--command", "node s.js", "--transport", "http"}, err: "--transport http needs --url"},
		{name: "bad transport", args: []string{"--url", "http://h/mcp", "--transport", "ws"}, err: "--transport must be auto, http or stdio"},
		{name: "bad command", args: []string{"--command", `node "x`}, err: "--command: unterminated"},
		{name: "command env without command", args: []string{"--url", "http://h/mcp", "--command-env", "A=1"}, err: "need --command"},
		{name: "command cwd without command", args: []string{"--url", "http://h/mcp", "--command-cwd", "."}, err: "need --command"},
		{name: "missing cwd", args: []string{"--command", "node s.js", "--command-cwd", "does-not-exist"}, err: "--command-cwd does-not-exist"},
		{name: "cwd is a file", args: []string{"--command", "node s.js", "--command-cwd", scen}, err: "not a directory"},
		{name: "bad command env", args: []string{"--command", "node s.js", "--command-env", "sk-value"}, err: "--command-env #1 must be KEY=VALUE"},
		{name: "env MCP_URL with command", args: []string{"--command", "node s.js", "--env", "MCP_URL=http://x"}, err: "--env MCP_URL"},
		{name: "env MCP_COMMAND with url", args: []string{"--url", "http://h/mcp", "--env", "MCP_COMMAND=[]"}, err: "--env MCP_COMMAND"},
		{name: "process sampler needs stdio", args: []string{"--url", "http://h/mcp", "--sampler", "process"}, err: "--sampler process"},
		{name: "chaos", args: []string{"--command", "node s.js", "--chaos-restart", "30s", "--chaos-container", "c"}, err: "--chaos-restart restarts a Docker container"},
		{name: "calls url", args: []string{"--command", "node s.js", "--calls-url", "http://h/calls"}, err: "--calls-url"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, err := parseRun(t, append([]string{"--scenario", scen}, c.args...)...)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if o.stdio != c.stdio {
				t.Errorf("stdio = %v", o.stdio)
			}
		})
	}
}

func TestStdioRejectsHTTPOnlyScenarios(t *testing.T) {
	for _, s := range []string{"lb-check", "version-skew", "scenarios/lb-check.js"} {
		_, err := parseRun(t, "--command", "node s.js", "--scenario", s)
		if err == nil || !strings.Contains(err.Error(), "does not apply to a stdio server") {
			t.Errorf("%s: err = %v", s, err)
		}
	}
}

func TestStdioDefaults(t *testing.T) {
	o, err := parseRun(t, "--scenario", "stdio_test.go", "--command", `node "my server.js" --token=s3cr3t`,
		"--command-env", "API_KEY=s3cr3t", "--command-env", "MODE=test", "--command-cwd", ".", "--label", "local")
	if err != nil {
		t.Fatal(err)
	}
	if o.samplerKind != report.SamplerProcess {
		t.Errorf("sampler = %q, want process", o.samplerKind)
	}
	if !reflect.DeepEqual(o.argv, []string{"node", "my server.js", "--token=s3cr3t"}) {
		t.Errorf("argv = %q", o.argv)
	}
	cwd, _ := os.Getwd()
	if o.commandCwd != cwd {
		t.Errorf("cwd = %q, want %q", o.commandCwd, cwd)
	}

	env, m := o.k6Env()
	var argv []string
	if err := json.Unmarshal([]byte(m["MCP_COMMAND"]), &argv); err != nil || !reflect.DeepEqual(argv, o.argv) {
		t.Errorf("MCP_COMMAND = %q", m["MCP_COMMAND"])
	}
	if v, ok := m["MCP_URL"]; !ok || v != "" {
		t.Errorf("MCP_URL must be passed empty, got %q (set %v)", v, ok)
	}
	if m["MCP_COMMAND_CWD"] != cwd {
		t.Errorf("MCP_COMMAND_CWD = %q", m["MCP_COMMAND_CWD"])
	}
	// The server env never goes on the k6 command line.
	if _, ok := m["MCP_COMMAND_ENV"]; ok {
		t.Error("MCP_COMMAND_ENV must not be a -e value")
	}
	for _, a := range (k6run.RunConfig{Env: env}).Args() {
		if strings.Contains(a, "API_KEY") {
			t.Errorf("k6 args leak the command env: %q", a)
		}
	}

	oenv := o.stdioOSEnv("/tmp/pids")
	var cenv map[string]string
	if len(oenv) != 2 || !strings.HasPrefix(oenv[0], "MCP_COMMAND_ENV=") || oenv[1] != sampler.PIDDirEnv+"=/tmp/pids" {
		t.Fatalf("os env = %q", oenv)
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(oenv[0], "MCP_COMMAND_ENV=")), &cenv); err != nil || cenv["API_KEY"] != "s3cr3t" || cenv["MODE"] != "test" {
		t.Errorf("MCP_COMMAND_ENV = %q", oenv[0])
	}

	tg := o.targetOf()
	if tg.Transport != report.TransportStdio || tg.URL != "" || tg.Label != "local" ||
		!reflect.DeepEqual(tg.Command, []string{"node", "my server.js", "--token=REDACTED"}) {
		t.Errorf("target = %+v", tg)
	}
	js, _ := json.Marshal(tg)
	if strings.Contains(string(js), "s3cr3t") || strings.Contains(string(js), "API_KEY") {
		t.Errorf("target leaks a secret: %s", js)
	}
}

func TestStdioOSEnvWithoutCommandEnv(t *testing.T) {
	o, err := parseRun(t, "--scenario", "stdio_test.go", "--command", "srv")
	if err != nil {
		t.Fatal(err)
	}
	if got := o.stdioOSEnv("d"); got[0] != "MCP_COMMAND_ENV={}" {
		t.Errorf("got %q", got)
	}
	if _, m := o.k6Env(); m["MCP_COMMAND_CWD"] != "" {
		t.Error("cwd passed without --command-cwd")
	}
}

func TestHTTPTargetAndEnv(t *testing.T) {
	o, err := parseRun(t, "--scenario", "stdio_test.go", "--url", "http://h/mcp?token=abc")
	if err != nil {
		t.Fatal(err)
	}
	if o.samplerKind != "none" {
		t.Errorf("sampler = %q", o.samplerKind)
	}
	_, m := o.k6Env()
	if m["MCP_URL"] != "http://h/mcp?token=abc" {
		t.Errorf("MCP_URL = %q", m["MCP_URL"])
	}
	if _, ok := m["MCP_COMMAND"]; ok {
		t.Error("MCP_COMMAND on an http run")
	}
	if tg := o.targetOf(); tg.URL != "http://h/mcp?token=REDACTED" || tg.Transport != report.TransportHTTP || tg.Command != nil {
		t.Errorf("target = %+v", tg)
	}
}

func TestStdioExplicitSamplerKept(t *testing.T) {
	o, err := parseRun(t, "--scenario", "stdio_test.go", "--command", "srv", "--sampler", "none")
	if err != nil || o.samplerKind != "none" {
		t.Fatalf("sampler %q, err %v", o.samplerKind, err)
	}
}

func TestCommandLookupWarning(t *testing.T) {
	o := &runOpts{argv: []string{"surely-not-a-real-program-xyz"}}
	if w := o.commandLookupWarning(); !strings.Contains(w, "not found on PATH") {
		t.Errorf("warning = %q", w)
	}
	o.cmdEnv = map[string]string{"PATH": "/x"}
	if w := o.commandLookupWarning(); w != "" {
		t.Errorf("PATH override: %q", w)
	}
	o = &runOpts{argv: []string{"./server"}}
	if w := o.commandLookupWarning(); w != "" {
		t.Errorf("path: %q", w)
	}
	o = &runOpts{argv: []string{"go"}}
	if w := o.commandLookupWarning(); w != "" {
		t.Errorf("go: %q", w)
	}
}

func TestCapacityAcceptsCommand(t *testing.T) {
	o := &runOpts{set: map[string]bool{}}
	c := &capacityOpts{}
	fs := capacityFlags(o, c, io.Discard)
	for _, name := range []string{"command", "command-env", "command-cwd", "transport"} {
		if fs.Lookup(name) == nil {
			t.Errorf("capacity lacks --%s", name)
		}
	}
	var out, errb bytes.Buffer
	code := Main([]string{"capacity", "--command", "node s.js", "--url", "http://h/mcp"}, &out, &errb)
	if code != ExitError || !strings.Contains(errb.String(), "mutually exclusive") {
		t.Errorf("code %d, stderr %s", code, errb.String())
	}
}

func TestProcessInput(t *testing.T) {
	if in := processInput(nil); in.Present {
		t.Error("nil stats present")
	}
	in := processInput(&k6run.ProcessStats{Spawns: k6run.Latency{Count: 3, P95: 40}, InvalidLines: 2, Exits: 3, Unexpected: 1,
		ExitCodes: []k6run.ExitCodeCount{{Code: "1", Count: 1}}})
	if !in.Present || in.Spawned != 3 || in.SpawnP95Ms != 40 || in.InvalidLines != 2 || in.Unexpected != 1 || len(in.ExitCodes) != 1 || in.ExitCodes[0].Code != "1" {
		t.Errorf("got %+v", in)
	}
	if !slices.Contains(httpOnlyScenarios, versionSkewScenario) {
		t.Error("version-skew must be http only")
	}
}
