package analysis

import (
	"strings"
	"testing"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

func TestStdoutPollutionVerdict(t *testing.T) {
	cases := []struct {
		in     ProcessInput
		status string
		msg    string
	}{
		{ProcessInput{}, report.StatusSkipped, "no stdio process metrics"},
		{ProcessInput{Present: true}, report.StatusPass, "only JSON-RPC messages"},
		{ProcessInput{Present: true, InvalidLines: 1}, report.StatusFail, "wrote 1 line to stdout that was not JSON-RPC"},
		{ProcessInput{Present: true, InvalidLines: 42}, report.StatusFail, "wrote 42 lines to stdout that were not JSON-RPC"},
	}
	for _, c := range cases {
		v := StdoutPollutionVerdict(c.in)
		if v.ID != report.VerdictStdoutPollution || v.Status != c.status || !strings.Contains(v.Message, c.msg) || v.Signal != "mcp_stdout_invalid_lines" {
			t.Errorf("%+v: got %+v", c.in, v)
		}
	}
}

func TestProcessExitVerdict(t *testing.T) {
	cases := []struct {
		in     ProcessInput
		status string
		msg    string
	}{
		{ProcessInput{}, report.StatusSkipped, "no stdio process metrics"},
		{ProcessInput{Present: true, Spawned: 10, SpawnP95Ms: 41.6, Exits: 10}, report.StatusPass,
			"No server process exited unexpectedly (10 started, spawn p95 42 ms; 10 exited when their session closed)."},
		{ProcessInput{Present: true}, report.StatusPass, "No server process exited unexpectedly."},
		{ProcessInput{Present: true, Exits: 5, Unexpected: 3, ExitCodes: []ExitCode{{"137", 2}, {"1", 1}}}, report.StatusFail,
			"3 server processes exited unexpectedly (crashed, or exited while the session was in use); exit codes: 137 ×2, 1 ×1."},
		{ProcessInput{Present: true, Unexpected: 1}, report.StatusFail, "1 server process exited unexpectedly"},
	}
	for _, c := range cases {
		v := ProcessExitVerdict(c.in)
		if v.ID != report.VerdictProcessExit || v.Status != c.status || !strings.Contains(v.Message, c.msg) {
			t.Errorf("%+v: got %+v", c.in, v)
		}
	}
}

func TestGeneratorVerdictStdioNote(t *testing.T) {
	r := &report.Report{}
	r.Summary.Iterations, r.Summary.DroppedIterations = report.I64(100), report.I64(0)
	r.Run.Generator = &report.Generator{Cores: 4, CPUAvgPct: report.F(10), CPUMaxPct: report.F(20)}
	if v := GeneratorVerdict(r); strings.Contains(v.Message, "share its CPU") {
		t.Errorf("http run has the stdio note: %s", v.Message)
	}
	r.Run.Target = report.Target{Command: []string{"node", "s.js"}, Transport: report.TransportStdio}
	v := GeneratorVerdict(r)
	if v.Status != report.StatusPass || !strings.Contains(v.Message, "share its CPU") {
		t.Errorf("stdio: %+v", v)
	}
	// A note, never a new failure mode; it is added to a skipped verdict too.
	r.Run.Generator, r.Summary.Iterations, r.Summary.DroppedIterations = nil, nil, nil
	v = GeneratorVerdict(r)
	if v.Status != report.StatusSkipped || !strings.Contains(v.Message, "share its CPU") {
		t.Errorf("stdio skipped: %+v", v)
	}
}
