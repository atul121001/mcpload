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
		{VUs: 5, First: at(5), Last: at(25), Reqs: 400, Errors: 4, Connects: 40, ConnectP95: ptrF(12),
			Tools: []k6run.ToolStats{{Name: "slow", Reqs: 100, ErrorRate: 0, P95: 320, P99: 400}}},
		{VUs: 10, First: at(30), Last: at(50), Reqs: 600, Errors: 300, Connects: 60, ConnectErrors: 30,
			Tools: []k6run.ToolStats{{Name: "slow", Reqs: 100, Errors: 100, ErrorRate: 1}}},
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
	var buf bytes.Buffer
	printSteps(&buf, cp)
	out := buf.String()
	for _, want := range []string{"     5  PASS", "    10  BREACH", "95% (saturated)", "max sustainable concurrency: 5 agents (budgets broke at 10)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
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
