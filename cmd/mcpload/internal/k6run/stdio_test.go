package k6run

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProcessStats(t *testing.T) {
	pt := func(metric string, v float64, tags string) string {
		return `{"metric":"` + metric + `","type":"Point","data":{"time":"2026-10-03T00:00:01Z","value":` +
			strconv.FormatFloat(v, 'f', -1, 64) + `,"tags":{"transport":"stdio"` + tags + `}}}`
	}
	lines := []string{
		`{"type":"Metric","metric":"mcp_process_exits","data":{"name":"mcp_process_exits","type":"counter"}}`,
		pt(MetricProcessSpawnDuration, 20, ``),
		pt(MetricProcessSpawnDuration, 40, ``),
		pt(MetricProcessSpawnDuration, 30, ``),
		pt(MetricProcessesOpen, 1, ``),
		pt(MetricProcessesOpen, 3, ``),
		pt(MetricProcessesOpen, 2, ``),
		pt(MetricStdoutInvalidLines, 1, ``),
		pt(MetricStdoutInvalidLines, 2, ``),
		pt(MetricProcessExits, 1, `,"expected":"true","exit_code":"0"`),
		pt(MetricProcessExits, 1, `,"expected":"false","exit_code":"1"`),
		pt(MetricProcessExits, 1, `,"expected":"false","exit_code":"137"`),
		pt(MetricProcessExits, 1, `,"expected":"false","exit_code":"137"`),
		pt(MetricProcessExits, 1, `,"expected":"false","exit_code":"2"`),
		pt(MetricProcessExits, 1, `,"expected":"false"`),
		pt(MetricReqs, 1, `,"method":"initialize"`),
	}
	origin, _ := time.Parse(time.RFC3339, "2026-10-03T00:00:00Z")
	a := NewAggregator(origin, 10*time.Second, nil)
	if err := a.Read(strings.NewReader(strings.Join(lines, "\n"))); err != nil {
		t.Fatal(err)
	}
	p := a.Processes()
	if p == nil {
		t.Fatal("Processes() = nil")
	}
	if p.Spawns.Count != 3 || p.Spawns.Max != 40 || p.MaxOpen != 3 || p.InvalidLines != 3 || p.Exits != 6 || p.Unexpected != 5 {
		t.Errorf("got %+v", p)
	}
	want := []ExitCodeCount{{"137", 2}, {"1", 1}, {"2", 1}, {"unknown", 1}}
	if !reflect.DeepEqual(p.ExitCodes, want) {
		t.Errorf("exit codes %v, want %v", p.ExitCodes, want)
	}
	if a.Summary().Reqs != 1 {
		t.Error("other metrics still counted")
	}

	b := NewAggregator(origin, 10*time.Second, nil)
	if err := b.Read(strings.NewReader(pt(MetricReqs, 1, ``))); err != nil {
		t.Fatal(err)
	}
	if b.Processes() != nil {
		t.Error("Processes() without process metrics")
	}
}

func TestRunConfigOSEnv(t *testing.T) {
	cmd := RunConfig{Bin: "k6", Script: "s.js", OSEnv: []string{"MCP_COMMAND_ENV={\"K\":\"secret\"}", "MCPLOAD_PID_DIR=/tmp/p"}}.command()
	n := len(cmd.Env)
	if n < 2 || cmd.Env[n-2] != "MCP_COMMAND_ENV={\"K\":\"secret\"}" || cmd.Env[n-1] != "MCPLOAD_PID_DIR=/tmp/p" {
		t.Errorf("env tail %q", cmd.Env[max(0, n-3):])
	}
	for _, a := range cmd.Args {
		if strings.Contains(a, "secret") {
			t.Errorf("secret on the command line: %q", cmd.Args)
		}
	}
}
