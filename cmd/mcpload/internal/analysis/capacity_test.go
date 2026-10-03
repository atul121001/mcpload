package analysis

import (
	"fmt"
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

func TestCapacityEstimate(t *testing.T) {
	few := step(10, 20, map[string][4]float64{"fast": {300, 0, 5, 9}, "slow": {5, 0, 900, 950}}) // slow: too few calls to judge
	saturated := slowAt(20, 1900)
	saturated.GeneratorCPUMaxPct = report.F(97)
	noSlow := step(10, 20, map[string][4]float64{"fast": {300, 0, 5, 9}})
	cases := []struct {
		name  string
		steps []report.Step
		est   int // 0 = nil
		basis string
	}{
		// slow p95 320 -> 1900 crosses 800 ms at 13.04; slow p99 400 -> 2280 crosses 2 s at 18.5: the lower wins.
		{"interpolates the first breached metric", []report.Step{healthy(5), healthy(10), slowAt(20, 1900)}, 13,
			"between 10 (held) and 20 (broke); linear interpolation of `slow` p95 to its 800 ms budget (the lowest of 2 breached metrics)"},
		// 200 + (800-500)/(1400-500)*200 = 266.7, 2 significant figures.
		{"rounds to 2 significant figures", []report.Step{slowAt(200, 500), slowAt(400, 1400)}, 270,
			"between 200 (held) and 400 (broke); linear interpolation of `slow` p95 to its 800 ms budget"},
		// fast errors 0% -> 5% cross 1% at 120.
		{"error rate", []report.Step{healthy(100), step(200, 20, map[string][4]float64{"fast": {300, 15, 5, 9}})}, 120,
			"between 100 (held) and 200 (broke); linear interpolation of `fast` error rate to its 1% budget"},
		{"already over budget at the held step", []report.Step{few, slowAt(20, 1000)}, 10,
			"linear interpolation of `slow` p95, already at its 800 ms budget at 10 (too few calls there to count as a breach)"},
		{"breached metric not measured at the held step", []report.Step{noSlow, slowAt(20, 1900)}, 10,
			"between 10 (held) and 20 (broke); no breached metric was measured at both steps, so this is the last step that held"},
		{"never reaches the breaking step", []report.Step{slowAt(10, 790), slowAt(11, 801)}, 10, "between 10 (held) and 11 (broke)"},
		{"nothing broke", []report.Step{healthy(10), healthy(20)}, 0, "nothing broke up to 20 agents (the highest step tested)"},
		{"first step broke", []report.Step{slowAt(10, 900), slowAt(20, 2000)}, 0, "the first step (10 agents) already broke budgets"},
		{"inconclusive", []report.Step{healthy(10), saturated}, 0, "inconclusive: the load generator was saturated at 20 agents"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cp, _ := CapacityVerdict(c.steps, capCfg())
			if got := ip(cp.EstimatedVUs); got != c.est {
				t.Errorf("estimate %d, want %d (%s)", got, c.est, cp.EstimateBasis)
			}
			if !strings.Contains(cp.EstimateBasis, c.basis) {
				t.Errorf("basis %q lacks %q", cp.EstimateBasis, c.basis)
			}
		})
	}
}

func TestRoundSig2(t *testing.T) {
	for x, want := range map[float64]int{1: 1, 7.4: 7, 9.6: 10, 14.6: 15, 99.4: 99, 137: 140, 254: 250, 1349: 1300, 26667: 27000} {
		if got := RoundSig2(x); got != want {
			t.Errorf("RoundSig2(%v) = %d, want %d", x, got, want)
		}
	}
}

func TestCapacityDegradation(t *testing.T) {
	withP95 := func(st report.Step, p95 float64) report.Step {
		st.P95Ms, st.P99Ms = report.F(p95), report.F(p95*1.5)
		return st
	}
	cases := []struct {
		name  string
		steps []report.Step
		want  string // "" = no mark
	}{
		{"p95 doubles while budgets hold", []report.Step{withP95(healthy(10), 200), withP95(healthy(20), 300), withP95(healthy(40), 460), slowAt(80, 1900)},
			"40: p95 460 ms, 2.3x the first step's 200 ms"},
		{"falls back to the slowest tool without p95Ms", []report.Step{slowAt(10, 300), slowAt(20, 650), slowAt(40, 900)},
			"20: p95 650 ms, 2.2x the first step's 300 ms"},
		{"error rate reaches half its budget", []report.Step{healthy(10), step(20, 0, map[string][4]float64{"fast": {500, 3, 5, 9}})},
			"20: `fast` error rate 0.6%, over half its 1% budget"},
		{"a flaky tool at its usual rate is not a degradation", []report.Step{healthy(10), step(20, 0, map[string][4]float64{"fast": {300, 0, 5, 9}, "flaky": {100, 11, 40, 80}})}, ""},
		{"the breaking step is not a degradation", []report.Step{withP95(healthy(10), 200), withP95(slowAt(20, 1900), 1900)}, ""},
		{"the first step is the baseline", []report.Step{withP95(healthy(10), 700)}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cp, _ := CapacityVerdict(c.steps, capCfg())
			got := ""
			if d := cp.Degradation; d != nil {
				got = fmt.Sprintf("%d: %s", d.VUs, d.Reason)
			}
			if got != c.want {
				t.Errorf("degradation %q, want %q", got, c.want)
			}
		})
	}
}

func TestCapacityFailure(t *testing.T) {
	down := step(40, 20, map[string][4]float64{"fast": {300, 180, 5, 9}})
	down.ErrorRate, down.ByErrorType = 0.6, map[string]int64{"timeout": 180}
	cases := []struct {
		name    string
		steps   []report.Step
		stopped bool
		abort   float64
		want    string
	}{
		{"error rate reaches ABORT_ERR_RATE", []report.Step{healthy(10), slowAt(20, 1900), down}, false, 0, "40: error rate 60% >= 50%, all `timeout`"},
		{"and the run stopped there", []report.Step{healthy(10), down}, true, 0, "40: error rate 60% >= 50%, all `timeout`; the run stopped here (ABORT_ERR_RATE)"},
		{"a lower ABORT_ERR_RATE", []report.Step{healthy(10), down}, false, 0.3, "40: error rate 60% >= 30%, all `timeout`"},
		{"stopped early below the rate", []report.Step{healthy(10), slowAt(20, 1900)}, true, 0, "20: the run stopped here: a VU saw more than 50% of its calls fail (ABORT_ERR_RATE)"},
		{"no tool calls", []report.Step{healthy(10), {VUs: 20, Reqs: 40, Errors: 4, ErrorRate: 0.1}}, false, 0, "20: no tools/call completed"},
		{"breach without failure", []report.Step{healthy(10), slowAt(20, 1900)}, false, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := capCfg()
			cfg.StoppedEarly, cfg.AbortErrRate = c.stopped, c.abort
			cp, _ := CapacityVerdict(c.steps, cfg)
			got := ""
			if f := cp.Failure; f != nil {
				got = fmt.Sprintf("%d: %s", f.VUs, f.Reason)
			}
			if got != c.want {
				t.Errorf("failure %q, want %q", got, c.want)
			}
		})
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
