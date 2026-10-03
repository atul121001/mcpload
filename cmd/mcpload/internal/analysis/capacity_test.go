package analysis

import (
	"strings"
	"testing"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

func capCfg() CapacityConfig {
	return CapacityConfig{
		Default:    report.ToolBudget{P95Ms: 800, P99Ms: 2000, ErrorRate: 0.01},
		Tools:      map[string]report.ToolBudget{"flaky": {P95Ms: 800, P99Ms: 2000, ErrorRate: 0.2}},
		ConnectP95: 1500,
	}
}

// step builds a synthetic step: tools as name -> {reqs, errors, p95, p99}.
func step(vus int, cpu float64, tools map[string][4]float64) report.Step {
	st := report.Step{VUs: vus, ConnectP95Ms: report.F(20), ConnectErrorRate: report.F(0)}
	if cpu > 0 {
		st.GeneratorCPUMaxPct = report.F(cpu)
	}
	for _, n := range []string{"fast", "flaky", "slow"} {
		t, ok := tools[n]
		if !ok {
			continue
		}
		st.Tools = append(st.Tools, report.StepTool{Name: n, Reqs: int64(t[0]), Errors: int64(t[1]), ErrorRate: t[1] / t[0], P95: report.F(t[2]), P99: report.F(t[3])})
		st.Reqs += int64(t[0])
		st.Errors += int64(t[1])
	}
	return st
}

func healthy(vus int) report.Step {
	return step(vus, 20, map[string][4]float64{"fast": {300, 0, 5, 9}, "flaky": {100, 10, 40, 80}, "slow": {100, 0, 320, 400}})
}

func slowAt(vus int, p95 float64) report.Step {
	return step(vus, 20, map[string][4]float64{"fast": {300, 0, 5, 9}, "slow": {100, 0, p95, p95 * 1.2}})
}

func TestCapacityVerdict(t *testing.T) {
	cases := []struct {
		name      string
		steps     []report.Step
		minAgents int
		stopped   bool
		status    string
		maxOK     int // 0 = nil
		breaking  int // 0 = nil
		inconcl   bool
		msg       []string
	}{
		{name: "all steps hold", steps: []report.Step{healthy(5), healthy(10), healthy(20)},
			status: report.StatusPass, maxOK: 20, msg: []string{"Held budgets at every step up to 20 agents"}},
		{name: "breaks at third step, informational", steps: []report.Step{healthy(10), healthy(25), slowAt(50, 1900), slowAt(100, 4000)},
			status: report.StatusWarn, maxOK: 25, breaking: 50, msg: []string{"Held budgets up to 25 agents; at 50 agents `slow` p95 1.9 s > 800 ms"}},
		{name: "target met below the break", steps: []report.Step{healthy(10), healthy(25), slowAt(50, 1900)}, minAgents: 25,
			status: report.StatusPass, maxOK: 25, breaking: 50, msg: []string{"Target MIN_AGENTS=25 met"}},
		{name: "target not met", steps: []report.Step{healthy(10), healthy(25), slowAt(50, 1900)}, minAgents: 50,
			status: report.StatusFail, maxOK: 25, breaking: 50, msg: []string{"MIN_AGENTS=50 not met"}},
		{name: "target between passing and breaking step", steps: []report.Step{healthy(10), healthy(25), slowAt(50, 1900)}, minAgents: 40,
			status: report.StatusWarn, maxOK: 25, breaking: 50, msg: []string{"add a step at 40"}},
		{name: "target above the highest step", steps: []report.Step{healthy(10), healthy(25)}, minAgents: 40,
			status: report.StatusWarn, maxOK: 25, msg: []string{"was not reached: the highest step tested was 25"}},
		{name: "first step breaks", steps: []report.Step{slowAt(10, 900), slowAt(20, 2000)},
			status: report.StatusFail, breaking: 10, msg: []string{"Broke budgets at the first step (10 agents)", "No sustainable concurrency"}},
		{name: "saturated generator at the break is inconclusive", steps: []report.Step{healthy(10), step(50, 97, map[string][4]float64{"slow": {100, 0, 1900, 2100}})},
			minAgents: 50, status: report.StatusWarn, maxOK: 10, breaking: 50, inconcl: true,
			msg: []string{"Inconclusive at 50 agents", "k6 up to 97% CPU", "Held budgets up to 10 agents", "not confirmed"}},
		{name: "saturated generator on a passing step is fine", steps: []report.Step{step(10, 97, map[string][4]float64{"fast": {300, 0, 5, 9}}), slowAt(20, 900)},
			status: report.StatusWarn, maxOK: 10, breaking: 20},
		{name: "flaky keeps its own error budget, fast does not", steps: []report.Step{healthy(10), step(20, 0, map[string][4]float64{"fast": {300, 6, 5, 9}, "flaky": {100, 15, 40, 80}})},
			status: report.StatusWarn, maxOK: 10, breaking: 20, msg: []string{"`fast` error rate 2% > 1%"}},
		{name: "too few calls are not judged", steps: []report.Step{step(10, 0, map[string][4]float64{"fast": {300, 0, 5, 9}, "slow": {4, 2, 5000, 5000}})},
			status: report.StatusPass, maxOK: 10},
		{name: "no tool calls at all breaks the step", steps: []report.Step{healthy(10), {VUs: 20, Reqs: 40, Errors: 40, ConnectErrorRate: report.F(1)}},
			status: report.StatusWarn, maxOK: 10, breaking: 20, msg: []string{"at 20 agents no tools/call completed", "(+1 more)"}},
		{name: "stopped early lists the steps not run", steps: []report.Step{healthy(5), step(10, 0, map[string][4]float64{"fast": {300, 200, 5, 9}})}, stopped: true,
			status: report.StatusWarn, maxOK: 5, breaking: 10, msg: []string{"stopped early at 10 agents", "steps not run: 20, 40"}},
		{name: "no steps", status: report.StatusSkipped},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := capCfg()
			cfg.MinAgents = c.minAgents
			cfg.StoppedEarly = c.stopped
			cfg.Planned = []int{5, 10, 20, 40}
			cp, v := CapacityVerdict(c.steps, cfg)
			if v.Status != c.status || v.ID != report.VerdictCapacity {
				t.Fatalf("status %s, want %s: %s", v.Status, c.status, v.Message)
			}
			if got := ip(cp.MaxSustainableVUs); got != c.maxOK {
				t.Errorf("maxSustainable %d, want %d: %s", got, c.maxOK, v.Message)
			}
			if got := ip(cp.BreakingVUs); got != c.breaking {
				t.Errorf("breaking %d, want %d: %s", got, c.breaking, v.Message)
			}
			if cp.Inconclusive != c.inconcl {
				t.Errorf("inconclusive %v: %s", cp.Inconclusive, v.Message)
			}
			for _, w := range c.msg {
				if !strings.Contains(v.Message, w) {
					t.Errorf("message %q lacks %q", v.Message, w)
				}
			}
		})
	}
}

func ip(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func TestCapacityMarksSteps(t *testing.T) {
	steps := []report.Step{healthy(10), slowAt(20, 1900)}
	steps[1].ConnectP95Ms = report.F(1600)
	cp, _ := CapacityVerdict(steps, capCfg())
	if !steps[0].Passed || len(steps[0].Breaches) != 0 || steps[1].Passed {
		t.Fatalf("passed flags: %+v", steps)
	}
	if got := strings.Join(steps[1].Breached, ","); got != "connectP95Ms" {
		t.Errorf("step breached = %s", got)
	}
	slow := steps[1].Tools[1]
	if slow.Name != "slow" || strings.Join(slow.Breached, ",") != "p95,p99" {
		t.Errorf("slow = %+v", slow)
	}
	// Worst breach first: slow p95 is 2.4x its budget, connect p95 1.07x.
	if steps[1].Breaches[0] != "`slow` p95 1.9 s > 800 ms" || len(steps[1].Breaches) != 3 {
		t.Errorf("breaches = %q", steps[1].Breaches)
	}
	if b := cp.Budgets.Tools["flaky"]; b.ErrorRate != 0.2 || cp.Budgets.Tools["slow"].P95Ms != 800 || cp.Budgets.ConnectP95Ms != 1500 {
		t.Errorf("budgets = %+v", cp.Budgets)
	}
	r := &report.Report{Capacity: cp}
	r.Normalize()
	if r.Capacity.Steps[0].Breaches == nil {
		t.Error("normalize should keep breaches non-nil")
	}
}

func TestFormatMs(t *testing.T) {
	for v, want := range map[float64]string{3.14: "3.1 ms", 800: "800 ms", 1900: "1.9 s", 2000: "2 s", 1234: "1.23 s"} {
		if got := FormatMs(v); got != want {
			t.Errorf("FormatMs(%v) = %q, want %q", v, got, want)
		}
	}
}

func TestSkipDriftForSteps(t *testing.T) {
	vs := SkipDriftForSteps([]report.Verdict{{ID: report.VerdictLatencyDrift, Status: report.StatusWarn, Signal: "tools.p95Ms"}, {ID: report.VerdictThreshold, Status: report.StatusPass}})
	if vs[0].Status != report.StatusSkipped || vs[0].Signal != "tools.p95Ms" || vs[1].Status != report.StatusPass {
		t.Errorf("verdicts = %+v", vs)
	}
}

func TestCapacityNamesErrorClass(t *testing.T) {
	broken := step(40, 20, map[string][4]float64{"fast": {300, 0, 5, 9}, "slow": {100, 12, 320, 400}})
	broken.Tools[1].ByErrorType = map[string]int64{"timeout": 9, "http": 3}
	broken.ByErrorType = map[string]int64{"timeout": 9, "http": 3}
	_, v := CapacityVerdict([]report.Step{healthy(20), broken}, capCfg())
	if want := "at 40 agents `slow` error rate 12% > 1%, mostly `timeout` (9 of 12)."; !strings.Contains(v.Message, want) {
		t.Errorf("message %q lacks %q", v.Message, want)
	}
	if got := dominantClass(map[string]int64{"http": 2}); got != ", all `http`" {
		t.Errorf("single class: %q", got)
	}
	if got := dominantClass(map[string]int64{"http": 2, "timeout": 2, "auth": 1}); got != ", mainly `http` (2 of 5)" {
		t.Errorf("no majority: %q", got)
	}
	if dominantClass(nil) != "" || TopErrorTypes(nil, 2) != "-" {
		t.Error("empty classes")
	}
	if got := TopErrorTypes(map[string]int64{"timeout": 14, "http": 3, "auth": 0}, 2); got != "timeout 14, http 3" {
		t.Errorf("top: %q", got)
	}
}
