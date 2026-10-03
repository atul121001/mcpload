package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/analysis"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/k6run"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/sampler"
)

func TestCapacityConfigMirrorsScenarioBudgets(t *testing.T) {
	cfg, err := capacityConfig(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Default != (report.ToolBudget{P95Ms: 800, P99Ms: 2000, ErrorRate: 0.01}) || cfg.ConnectP95 != 1500 || cfg.MinAgents != 0 {
		t.Errorf("defaults = %+v", cfg)
	}
	if b := cfg.Budget("flaky"); b.ErrorRate != 0.2 || b.P95Ms != 800 {
		t.Errorf("flaky = %+v", b)
	}
	cfg, err = capacityConfig(map[string]string{"P95_MS": "500", "ERR_RATE": "0.05", "MIN_AGENTS": "40",
		"TOOL_BUDGETS": `{"slow":{"p95":1500},"flaky":{"p99":3000}}`})
	if err != nil {
		t.Fatal(err)
	}
	if b := cfg.Budget("slow"); b.P95Ms != 1500 || b.P99Ms != 2000 || b.ErrorRate != 0.05 {
		t.Errorf("slow = %+v", b)
	}
	// Like Object.assign in lib/config.js, a TOOL_BUDGETS entry replaces the built-in flaky one.
	if b := cfg.Budget("flaky"); b.ErrorRate != 0.05 || b.P99Ms != 3000 || b.P95Ms != 500 {
		t.Errorf("flaky = %+v", b)
	}
	if cfg.Budget("other").P95Ms != 500 || cfg.MinAgents != 40 {
		t.Errorf("cfg = %+v", cfg)
	}
	for _, env := range []map[string]string{{"P95_MS": "fast"}, {"MIN_AGENTS": "2.5"}, {"MIN_AGENTS": "-1"}, {"TOOL_BUDGETS": "[1]"}} {
		if _, err := capacityConfig(env); err == nil {
			t.Errorf("%v: want an error", env)
		}
	}
}

func TestBuildStepsAndPrint(t *testing.T) {
	t0 := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	in := []k6run.StepStats{
		{VUs: 5, First: at(5), Last: at(25), Reqs: 400, Errors: 4, Connects: 40, ConnectP95: ptrF(12), CallP95: ptrF(300), CallP99: ptrF(390),
			Tools: []k6run.ToolStats{{Name: "slow", Reqs: 100, ErrorRate: 0, P95: 320, P99: 400}}},
		{VUs: 10, First: at(30), Last: at(50), Reqs: 600, Errors: 300, Connects: 60, ConnectErrors: 30,
			Tools:       []k6run.ToolStats{{Name: "slow", Reqs: 100, Errors: 100, ErrorRate: 1, ByErrorType: map[string]int64{"timeout": 90, "http": 10}}},
			ByErrorType: map[string]int64{"timeout": 260, "http": 30, "session_not_found": 10}},
	}
	cpu := []sampler.CPUWindow{
		{Start: at(0), End: at(8), Pct: 99}, // mostly ramp: midpoint before step 5
		{Start: at(8), End: at(20), Pct: 30},
		{Start: at(20), End: at(30), Pct: 95}, // midpoint 25: still step 5
		{Start: at(30), End: at(40), Pct: 50},
	}
	steps := buildSteps(in, t0, cpu)
	s5, s10 := steps[0], steps[1]
	if s5.StartS != 5 || s5.EndS != 25 || s5.RPS != 20 || s5.ErrorRate != 0.01 || *s5.ConnectErrorRate != 0 || *s5.GeneratorCPUMaxPct != 95 {
		t.Errorf("step 5 = %+v", s5)
	}
	if s5.Tools[0].P95 == nil || *s5.Tools[0].P95 != 320 {
		t.Errorf("step 5 tools = %+v", s5.Tools)
	}
	if s10.ConnectP95Ms != nil || *s10.ConnectErrorRate != 0.5 || s10.Tools[0].P95 != nil || *s10.GeneratorCPUMaxPct != 50 {
		t.Errorf("step 10 = %+v", s10)
	}
	cfg, _ := capacityConfig(map[string]string{})
	cp, v := analysis.CapacityVerdict(steps, cfg)
	if v.Status != report.StatusWarn || *cp.MaxSustainableVUs != 5 {
		t.Fatalf("verdict %s: %s", v.Status, v.Message)
	}
	if s5.P95Ms == nil || *s5.P95Ms != 300 || *s5.P99Ms != 390 || s10.P95Ms != nil {
		t.Errorf("overall p95: step 5 %v, step 10 %v", s5.P95Ms, s10.P95Ms)
	}
	var buf bytes.Buffer
	printSteps(&buf, cp, capacityStepHints)
	out := buf.String()
	for _, want := range []string{
		"  Agents     p95     p99  Errors  req/s  Slowest tool p95\n",
		"       5  300 ms  390 ms   1.00%   20.0  slow 320 ms       (k6 CPU 95%: load generator saturated)\n",
		"      10       -       -  50.00%   30.0  -                 <- breaks budget: `slow` error rate 100% > 1%, mostly `timeout` (90 of 100) (+1 more); <- failure: error rate 50% >= 50%, mostly `timeout` (260 of 300)\n",
		"max sustainable concurrency: 5 agents (budgets broke at 10)",
		"Estimated sustainable capacity: ~5 agents (between 5 (held) and 10 (broke); linear interpolation of `slow` error rate to its 1% budget (the lowest of 2 breached metrics); an estimate, not a measured step)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestEstimateLine(t *testing.T) {
	step := func(vus int) report.Step { return report.Step{VUs: vus} }
	cases := []struct {
		name string
		c    report.Capacity
		h    stepHints
		want string
	}{
		{"interpolated", report.Capacity{EstimatedVUs: report.I(250), EstimateBasis: "between 200 (held) and 400 (broke); linear interpolation of `slow` p95 to its 800 ms budget",
			MaxSustainableVUs: report.I(200), BreakingVUs: report.I(400), Steps: []report.Step{step(200), step(400)}}, capacityStepHints,
			"~250 agents (between 200 (held) and 400 (broke); linear interpolation of `slow` p95 to its 800 ms budget; an estimate, not a measured step)"},
		{"nothing broke", report.Capacity{MaxSustainableVUs: report.I(80), Steps: []report.Step{step(10), step(80)}}, capacityStepHints,
			"at least 80 agents (nothing broke; raise --to)"},
		{"nothing broke, run", report.Capacity{MaxSustainableVUs: report.I(80), Steps: []report.Step{step(80)}}, runStepHints,
			"at least 80 agents (nothing broke; add higher STEPS)"},
		{"first step broke", report.Capacity{BreakingVUs: report.I(5), Steps: []report.Step{step(5), step(10)}}, capacityStepHints,
			"below 5 agents (the first step broke; lower --from)"},
		{"inconclusive", report.Capacity{MaxSustainableVUs: report.I(10), BreakingVUs: report.I(20), Inconclusive: true, Steps: []report.Step{step(10), step(20)}}, capacityStepHints,
			"inconclusive (the load generator was saturated at 20 agents; run k6 on a separate or bigger machine)"},
	}
	for _, tc := range cases {
		if got := estimateLine(&tc.c, tc.h); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
}

func TestStepNotes(t *testing.T) {
	c := &report.Capacity{BreakingVUs: report.I(40), Degradation: &report.StepMark{VUs: 20, Reason: "p95 1.1 s, 2.2x the first step's 500 ms"},
		Failure: &report.StepMark{VUs: 80, Reason: "error rate 62% >= 50%, mostly `timeout` (90 of 100)"}}
	cases := []struct {
		st   report.Step
		want string
	}{
		{report.Step{VUs: 10, Passed: true}, ""},
		{report.Step{VUs: 20, Passed: true}, "<- degradation: p95 1.1 s, 2.2x the first step's 500 ms"},
		{report.Step{VUs: 40, Breaches: []string{"`slow` p95 1.36 s > 800 ms", "`big` p95 900 ms > 800 ms"}}, "<- breaks budget: `slow` p95 1.36 s > 800 ms (+1 more)"},
		{report.Step{VUs: 60, Breaches: []string{"`slow` p95 3 s > 800 ms"}}, "over budget: `slow` p95 3 s > 800 ms"},
		{report.Step{VUs: 80, Breaches: []string{"`slow` error rate 62% > 1%"}}, "<- failure: error rate 62% >= 50%, mostly `timeout` (90 of 100)"},
	}
	for _, tc := range cases {
		if got := strings.Join(stepNotes(c, tc.st), "; "); got != tc.want {
			t.Errorf("step %d: got %q, want %q", tc.st.VUs, got, tc.want)
		}
	}
}

func ptrF(v float64) *float64 { return &v }

func TestMinAgentsFlagReachesK6(t *testing.T) {
	o := &runOpts{url: "http://x/mcp", protocol: "auto", minAgents: 50, set: map[string]bool{"min-agents": true}}
	_, m := o.k6Env()
	if m["MIN_AGENTS"] != "50" {
		t.Errorf("MIN_AGENTS = %q", m["MIN_AGENTS"])
	}
}
