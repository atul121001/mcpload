package analysis

import (
	"strings"
	"testing"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

// cmpRun is a 60 s agent-session run with the given tools; summary totals
// are the tools' sums.
func cmpRun(tools ...report.ToolStats) *report.Report {
	r := &report.Report{Run: report.Run{ID: "run-1234567890", Scenario: "agent-session", Protocol: "2025-11-25", DurationS: 60,
		Load: report.Load{Executor: "constant-vus", VUs: report.I(10)}},
		Phases: report.Phases{LoadEndS: 60, CooldownEndS: 60}, Tools: tools}
	r.Series.Server.Sampler = report.SamplerNone
	for _, t := range tools {
		r.Summary.Reqs += t.Reqs
		r.Summary.Errors += t.Errors
	}
	r.Summary.ErrorRate = ratioOf64(r.Summary.Errors, r.Summary.Reqs)
	return r
}

// tool is a tool with p50 = p95/2 and p99 = 1.2 × p95.
func tool(name string, reqs, errs int64, p95 float64) report.ToolStats {
	return report.ToolStats{Name: name, Reqs: reqs, Errors: errs, ErrorRate: ratioOf64(errs, reqs), P50: p95 / 2, P95: p95, P99: p95 * 1.2, Max: p95 * 2}
}

func findTool(c *report.Comparison, name string) report.ToolDelta {
	for _, t := range c.Tools {
		if t.Name == name {
			return t
		}
	}
	return report.ToolDelta{}
}

func findMetric(c *report.Comparison, id string) *report.MetricDelta {
	for i := range c.Metrics {
		if c.Metrics[i].ID == id {
			return &c.Metrics[i]
		}
	}
	return nil
}

// The per-tool noise rules: relative AND absolute latency floors, minimum
// calls, and the error-rate floor plus two-proportion test.
func TestCompareToolRules(t *testing.T) {
	cases := []struct {
		name      string
		base, cur report.ToolStats
		status    string
		regressed []string
	}{
		{"p95 +35% and +74 ms", tool("search", 1000, 1, 210), tool("search", 1000, 1, 284), report.DeltaRegressed, []string{"p95", "p99"}},
		{"p95 +3% within noise", tool("search", 1000, 1, 180), tool("search", 1000, 1, 185), report.DeltaOK, nil},
		{"fast tool +50% but only +3 ms", tool("fast", 1000, 0, 6), tool("fast", 1000, 0, 9), report.DeltaOK, nil},
		{"slow tool +30 ms but only +10%", tool("slow", 1000, 0, 300), tool("slow", 1000, 0, 330), report.DeltaOK, nil},
		{"p99 only: +25% p99 is under its 30% limit", report.ToolStats{Name: "x", Reqs: 500, P50: 10, P95: 100, P99: 400, Max: 900},
			report.ToolStats{Name: "x", Reqs: 500, P50: 10, P95: 100, P99: 500, Max: 900}, report.DeltaOK, nil},
		{"p99 only: +40% p99", report.ToolStats{Name: "x", Reqs: 500, P50: 10, P95: 100, P99: 400, Max: 900},
			report.ToolStats{Name: "x", Reqs: 500, P50: 10, P95: 100, P99: 560, Max: 900}, report.DeltaRegressed, []string{"p99"}},
		{"too few calls", tool("rare", 30, 0, 100), tool("rare", 30, 0, 900), report.DeltaFewCalls, nil},
		{"errors 0/1000 -> 10/1000 (+1 pt, z 3.2)", tool("e", 1000, 0, 50), tool("e", 1000, 10, 50), report.DeltaRegressed, []string{"errorRate"}},
		{"errors 0/60 -> 1/60 (+1.7 pts, z 1.0: chance)", tool("e", 60, 0, 50), tool("e", 60, 1, 50), report.DeltaOK, nil},
		// Seen on CI: the demo's 10%-flaky tool between two runs of the same commit.
		{"errors 33/417 -> 52/432 (+4.1 pts, z 2.2: chance)", tool("flaky", 417, 33, 50), tool("flaky", 432, 52, 50), report.DeltaOK, nil},
		{"errors 1.0% -> 1.4% on 10k calls (under the 0.5 pt floor)", tool("e", 10000, 100, 50), tool("e", 10000, 140, 50), report.DeltaOK, nil},
		{"errors 10% -> 11% (+1 pt but only +10%)", tool("e", 5000, 500, 50), tool("e", 5000, 550, 50), report.DeltaOK, nil},
		{"errors 2% -> 4% on 5k calls", tool("e", 5000, 100, 50), tool("e", 5000, 200, 50), report.DeltaRegressed, []string{"errorRate"}},
		{"p95 300 -> 200 ms improved", tool("search", 1000, 0, 300), tool("search", 1000, 0, 200), report.DeltaImproved, nil},
		{"all calls failed: latency not judged", tool("dead", 100, 0, 50), report.ToolStats{Name: "dead", Reqs: 100, Errors: 100, ErrorRate: 1}, report.DeltaRegressed, []string{"errorRate"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Compare(cmpRun(tc.base), cmpRun(tc.cur), "base.json", DefaultCompareConfig(), DefaultConfig())
			td := c.Tools[0]
			t.Logf("%+v reasons=%v", td, c.Reasons)
			if td.Status != tc.status || strings.Join(td.Regressed, ",") != strings.Join(tc.regressed, ",") {
				t.Fatalf("status %s regressed %v, want %s %v", td.Status, td.Regressed, tc.status, tc.regressed)
			}
			if c.Regressed != (tc.status == report.DeltaRegressed) {
				t.Fatalf("Regressed = %v", c.Regressed)
			}
		})
	}
}

func TestCompareAddedRemoved(t *testing.T) {
	base := cmpRun(tool("search", 1000, 0, 100), tool("old", 1000, 0, 100))
	cur := cmpRun(tool("search", 1000, 0, 100), tool("new", 1000, 900, 5000))
	c := Compare(base, cur, "base.json", DefaultCompareConfig(), DefaultConfig())
	if a, r := findTool(c, "new"), findTool(c, "old"); a.Status != report.DeltaAdded || a.Base != nil || r.Status != report.DeltaRemoved || r.Current != nil {
		t.Fatalf("added %+v removed %+v", a, r)
	}
	if c.Tools[2].Name != "old" {
		t.Errorf("removed tools come last: %v", c.Tools)
	}
	if findTool(c, "search").Status != report.DeltaOK {
		t.Errorf("search: %+v", findTool(c, "search"))
	}
	// The new tool's errors still raise the overall error rate, which is judged.
	if m := findMetric(c, MetricErrorRate); m == nil || m.Status != report.DeltaRegressed || !c.Regressed {
		t.Fatalf("overall error rate: %+v", m)
	}
	v := RegressionVerdict(c, true)
	if v.Status != report.StatusFail || !strings.Contains(v.Message, "error rate 0% → 45%") {
		t.Errorf("verdict: %+v", v)
	}
}

func TestCompareMismatchWarns(t *testing.T) {
	base := cmpRun(tool("search", 1000, 0, 100))
	cur := cmpRun(tool("search", 1000, 0, 100))
	cur.Run.Scenario, cur.Run.Protocol, cur.Run.Load.VUs = "burst", "2026-07-28", report.I(50)
	cur.Phases.LoadEndS, cur.Phases.CooldownEndS = 120, 120
	cur.Verdicts = []report.Verdict{{ID: report.VerdictGenerator, Status: report.StatusWarn}}
	c := Compare(base, cur, "base.json", DefaultCompareConfig(), DefaultConfig())
	w := strings.Join(c.Warnings, "\n")
	for _, want := range []string{"scenario differs (baseline agent-session, current burst)", "protocol differs", "load differs (baseline constant-vus 10 VUs, current constant-vus 50 VUs)",
		"load duration differs (baseline 60 s, current 120 s)", "load generator was saturated in the current run", "may be unfair"} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings missing %q:\n%s", want, w)
		}
	}
	if c.Regressed {
		t.Error("a mismatch alone is not a regression")
	}
	v := RegressionVerdict(c, true)
	if v.Status != report.StatusWarn || !strings.Contains(v.Message, "No regression") || !strings.Contains(v.Message, "Note: scenario differs") {
		t.Errorf("verdict: %+v", v)
	}
	same := Compare(base, cmpRun(tool("search", 1000, 0, 100)), "base.json", DefaultCompareConfig(), DefaultConfig())
	if len(same.Warnings) != 0 || RegressionVerdict(same, true).Status != report.StatusPass {
		t.Errorf("identical runs: %v", same.Warnings)
	}
}

func TestRegressionVerdict(t *testing.T) {
	c := Compare(cmpRun(tool("search", 1000, 1, 210)), cmpRun(tool("search", 1000, 1, 284)), "base.json", DefaultCompareConfig(), DefaultConfig())
	c.Baseline.Git = &report.Git{SHA: "abcdef1234567", Ref: "refs/heads/main"}
	v := RegressionVerdict(c, true)
	want := "Performance regression vs baseline abcdef1 (main): `search` p95 210 ms → 284 ms (+35%, +74 ms); `search` p99 252 ms → 341 ms (+35%, +89 ms)."
	if v.Status != report.StatusFail || v.Message != want || v.ID != report.VerdictRegression {
		t.Errorf("got %s %q\nwant %q", v.Status, v.Message, want)
	}
	if v := RegressionVerdict(c, false); v.Status != report.StatusWarn {
		t.Errorf("fail-on-regression=false: %s", v.Status)
	}
	ok := Compare(cmpRun(tool("a", 1000, 0, 100), tool("b", 10, 0, 1)), cmpRun(tool("a", 1000, 0, 60), tool("b", 10, 0, 1), tool("c", 100, 0, 1)), "base.json", DefaultCompareConfig(), DefaultConfig())
	v = RegressionVerdict(ok, true)
	want = "No regression vs baseline run run-1234: 0 of 3 tools within noise, 1 improved, 1 with fewer than 50 calls (not judged), added `c`."
	if v.Status != report.StatusPass || v.Message != want {
		t.Errorf("got %s %q\nwant %q", v.Status, v.Message, want)
	}
}

func TestCompareConnectAndCapacity(t *testing.T) {
	base, cur := cmpRun(tool("a", 1000, 0, 100)), cmpRun(tool("a", 1000, 0, 100))
	base.Thresholds = []report.Threshold{{Metric: "mcp_connect_duration", Expr: "p(95)<1500", Passed: true, Observed: report.F(40)}}
	cur.Thresholds = []report.Threshold{{Metric: "mcp_connect_duration", Expr: "p(95)<1500", Passed: true, Observed: report.F(90)}}
	base.Capacity = &report.Capacity{MaxSustainableVUs: report.I(50)}
	cur.Capacity = &report.Capacity{MaxSustainableVUs: report.I(25)}
	c := Compare(base, cur, "b", DefaultCompareConfig(), DefaultConfig())
	if m := findMetric(c, MetricConnectP95); m == nil || m.Status != report.DeltaRegressed {
		t.Errorf("connect p95 40 -> 90 ms: %+v", m)
	}
	if m := findMetric(c, MetricMaxSustainable); m == nil || m.Status != report.DeltaRegressed || *m.Current != 25 {
		t.Errorf("capacity 50 -> 25: %+v", m)
	}
	cur.Capacity.Inconclusive = true
	c = Compare(base, cur, "b", DefaultCompareConfig(), DefaultConfig())
	if m := findMetric(c, MetricMaxSustainable); m.Status != report.DeltaOK || !strings.Contains(m.Note, "inconclusive") {
		t.Errorf("inconclusive capacity: %+v", m)
	}
	cur.Capacity.MaxSustainableVUs, cur.Capacity.Inconclusive = nil, false // the first step broke
	c = Compare(base, cur, "b", DefaultCompareConfig(), DefaultConfig())
	if m := findMetric(c, MetricMaxSustainable); m.Status != report.DeltaRegressed || *m.Current != 0 {
		t.Errorf("first step broke: %+v", m)
	}
}

// memRun is a soak-shaped run (warm-up 60 s, load to 600 s, cool-down to
// 900 s, 30 s buckets) whose RSS grows growth MiB/min under load and keeps
// retain MiB after it.
func memRun(growth, retain float64) *report.Report {
	r := synth(60, 600, 900)
	r.Run.Scenario, r.Run.Protocol, r.Run.Load = "soak", "2025-11-25", report.Load{Executor: "constant-arrival-rate"}
	n := noise(3)
	r.Series.Server.RSSBytes = gen(r, func(i int, t float64) float64 {
		v := 100.0
		switch {
		case t >= 60 && t < 600:
			v += growth * (t - 60) / 60
		case t >= 600:
			v += retain
		}
		return (v + 0.2*n()) * mib
	})
	return r
}

func TestCompareMemory(t *testing.T) {
	cfg, cc := DefaultConfig(), DefaultCompareConfig()
	flat := memRun(0, 0)
	c := Compare(flat, memRun(0, 0), "b", cc, cfg)
	for _, id := range []string{MetricMemoryGrowth, MetricLeakSlope, MetricRetained} {
		if m := findMetric(c, id); m == nil || m.Status != report.DeltaOK {
			t.Errorf("flat vs flat %s: %+v", id, m)
		}
	}
	leaky := memRun(3, 30) // 3 MiB/min for 9 min, 30 MiB kept
	c = Compare(flat, leaky, "b", cc, cfg)
	for _, id := range []string{MetricMemoryGrowth, MetricLeakSlope, MetricRetained} {
		if m := findMetric(c, id); m == nil || m.Status != report.DeltaRegressed {
			t.Errorf("flat vs leaky %s: %+v", id, m)
		}
	}
	if !c.Regressed {
		t.Error("want regressed")
	}
	// A slow rise under the slope limit and the growth floor is noise.
	c = Compare(flat, memRun(0.3, 1), "b", cc, cfg)
	if c.Regressed {
		t.Errorf("0.3 MiB/min: %v", c.Reasons)
	}
	// memory_leak going from pass to fail is a regression on its own.
	base, cur := memRun(0, 0), memRun(0, 0)
	base.Verdicts = []report.Verdict{{ID: report.VerdictMemoryLeak, Status: report.StatusPass}}
	cur.Verdicts = []report.Verdict{{ID: report.VerdictMemoryLeak, Status: report.StatusFail}}
	if m := findMetric(Compare(base, cur, "b", cc, cfg), MetricMemoryGrowth); m.Status != report.DeltaRegressed || m.Note != "memory_leak pass → fail" {
		t.Errorf("leak verdict worse: %+v", m)
	}
	// Short runs show the growth but do not judge it.
	short := func(growth float64) *report.Report {
		r := synth(0, 90, 90)
		r.Series.Server.RSSBytes = gen(r, func(i int, t float64) float64 { return (100 + growth*t/90) * mib })
		return r
	}
	m := findMetric(Compare(short(0), short(20), "b", cc, cfg), MetricMemoryGrowth)
	if m == nil || m.Status != report.DeltaNA || m.Base == nil || m.Current == nil || !strings.Contains(m.Note, "not judged") {
		t.Errorf("short runs: %+v", m)
	}
	// Different samplers measure RSS differently: memory is not compared.
	docker := memRun(3, 30)
	docker.Series.Server.Sampler = report.SamplerDocker
	c = Compare(flat, docker, "b", cc, cfg)
	if findMetric(c, MetricMemoryGrowth) != nil || !strings.Contains(strings.Join(c.Warnings, " "), "sampler differs") {
		t.Errorf("sampler mismatch: %v %v", c.Metrics, c.Warnings)
	}
}

func TestFormatDelta(t *testing.T) {
	for _, tc := range []struct {
		unit string
		d    float64
		want string
	}{{"ms", 74, "+74 ms"}, {"ms", -7.8, "-7.8 ms"}, {"ms", 0.01, "0 ms"}, {"ms", 1500, "+1.5 s"}, {"rate", 0.007, "+0.70 pts"},
		{"rate", 0, "0 pts"}, {"MiB", 4.6, "+4.6 MiB"}, {"MiB/min", -1.25, "-1.25 MiB/min"}, {"agents", -25, "-25 agents"}} {
		if got := FormatDelta(tc.unit, tc.d); got != tc.want {
			t.Errorf("FormatDelta(%s, %v) = %q, want %q", tc.unit, tc.d, got, tc.want)
		}
	}
}
