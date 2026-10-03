package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const repoRoot = "../../../../"

func examplePath(name string) string { return filepath.Join(repoRoot, "report", "examples", name) }

func TestEmbeddedTemplateInSync(t *testing.T) {
	src := filepath.Join(repoRoot, "report", "template", "report.html")
	b, err := os.ReadFile(src)
	if os.IsNotExist(err) {
		t.Skip("report/template/report.html not present")
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != TemplateHTML() {
		t.Fatalf("internal/report/template.html differs from %s; copy it over", src)
	}
}

func TestRoundTripValidates(t *testing.T) {
	dir := t.TempDir()
	var outs []string
	for _, name := range []string{"healthy.json", "leaky.json", "step-load.json"} {
		r, err := ReadJSON(examplePath(name))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Check(); err != nil {
			t.Fatalf("%s: Check: %v", name, err)
		}
		out := filepath.Join(dir, name)
		if err := WriteJSON(out, r); err != nil {
			t.Fatal(err)
		}
		r2, err := ReadJSON(out)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(r)
		b, _ := json.Marshal(r2)
		if !bytes.Equal(a, b) {
			t.Fatalf("%s: round trip changed content", name)
		}
		outs = append(outs, out)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found; skipping validate.mjs")
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "report", "node_modules", "ajv")); err != nil {
		t.Skip("report/node_modules not installed; skipping validate.mjs")
	}
	cmd := exec.Command(node, append([]string{filepath.Join(repoRoot, "report", "validate.mjs")}, outs...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("validate.mjs failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

func TestCheckCatchesErrors(t *testing.T) {
	r, err := ReadJSON(examplePath("healthy.json"))
	if err != nil {
		t.Fatal(err)
	}
	r.Series.Client.RPS = r.Series.Client.RPS[1:]
	r.Phases.LoadEndS = r.Phases.CooldownEndS + 1
	r.Summary.Errors++
	r.Tools[0].P50 = r.Tools[0].Max + 1
	err = r.Check()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"client.rps", "phases", "byErrorType", "percentiles"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestCheckWorkflow(t *testing.T) {
	r, err := ReadJSON(examplePath("healthy.json"))
	if err != nil {
		t.Fatal(err)
	}
	r.Workflow = &Workflow{Runs: 4, Completed: 3, CompletionRate: 0.75, DurationMs: Latency{Count: 3, P50: 900, P95: 1400, P99: 1490, Max: 1500}}
	r.Normalize()
	if r.Workflow.Steps == nil {
		t.Fatal("Normalize left workflow.steps nil")
	}
	r.Workflow.Steps = append(r.Workflow.Steps, WorkflowStep{Name: "gather", Latency: Latency{Count: 4, P50: 5, P95: 9, P99: 9, Max: 9}})
	if err := r.Check(); err != nil {
		t.Fatalf("valid workflow: %v", err)
	}
	b, _ := Marshal(r)
	if !bytes.Contains(b, []byte(`"name": "gather",`)) || !bytes.Contains(b, []byte(`"count": 4,`)) {
		t.Errorf("step latency not flattened into the step object:\n%s", b)
	}

	r.Workflow.Completed = 5
	r.Workflow.Steps = append(r.Workflow.Steps, WorkflowStep{Name: "gather", Latency: Latency{Count: 1, P50: 9, P95: 5, P99: 5, Max: 5}})
	err = r.Check()
	for _, want := range []string{"workflow.completed > workflow.runs", "duplicate workflow step", "workflow.steps[gather] percentiles"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestNormalizeEmptyReportValidates(t *testing.T) {
	r := &Report{
		Tool:   ToolInfo{Name: "mcpload", Version: "0.0.0"},
		Run:    Run{ID: "x", StartedAt: "2026-01-01T00:00:00Z", EndedAt: "2026-01-01T00:00:01Z", Scenario: "soak", Protocol: "2025-06-18", Target: Target{URL: "http://localhost/mcp"}, K6Version: "v1", Load: Load{Executor: "constant-vus"}},
		Series: Series{IntervalS: 1},
	}
	b, err := Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Check(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"byErrorType": {}`, `"tools": []`, `"rssBytes": []`, `"sampler": "none"`} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("missing %s in\n%s", want, b)
		}
	}
}

func TestPassed(t *testing.T) {
	h, _ := ReadJSON(examplePath("healthy.json"))
	l, _ := ReadJSON(examplePath("leaky.json"))
	if !Passed(h) {
		t.Error("healthy should pass")
	}
	if Passed(l) {
		t.Error("leaky should fail")
	}
	h.Thresholds[0].Passed = false
	if Passed(h) {
		t.Error("failed threshold should fail")
	}
}

func TestRenderHTML(t *testing.T) {
	r, err := ReadJSON(examplePath("leaky.json"))
	if err != nil {
		t.Fatal(err)
	}
	r.Run.Target.Label = "a</script><b>&"
	var buf bytes.Buffer
	if err := RenderHTML(r, &buf); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	m := regexp.MustCompile(`(?s)<script type="application/json" id="report-data">(.*?)</script>`).FindStringSubmatch(html)
	if m == nil {
		t.Fatal("no report-data script")
	}
	var back Report
	if err := json.Unmarshal([]byte(m[1]), &back); err != nil {
		t.Fatalf("embedded JSON invalid: %v", err)
	}
	if back.Run.ID != r.Run.ID || back.Run.Target.Label != r.Run.Target.Label {
		t.Fatal("embedded JSON mismatch")
	}
	if strings.Contains(m[1], "<") || strings.Contains(m[1], ">") || strings.Contains(m[1], "&") {
		t.Fatal("unescaped <>& in embedded JSON")
	}
	if !strings.Contains(html, "<title>mcpload · a/scriptb · soak</title>") {
		t.Fatalf("bad title: %s", regexp.MustCompile(`<title>.*</title>`).FindString(html))
	}
}

// nodeValidator returns a function running report/validate.mjs on a file, or nil when node/ajv are unavailable.
func nodeValidator(t *testing.T) func(path string) (bool, string) {
	node, err := exec.LookPath("node")
	if err != nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "report", "node_modules", "ajv")); err != nil {
		return nil
	}
	return func(path string) (bool, string) {
		out, err := exec.Command(node, filepath.Join(repoRoot, "report", "validate.mjs"), path).CombinedOutput()
		return err == nil, string(out)
	}
}

// Check must be as strict as validate.mjs + the JSON schema: every mutation
// below is rejected by both, and the new optional fields are accepted by both.
func TestCheckStrictMatchesValidateMjs(t *testing.T) {
	validate := nodeValidator(t)
	dir := t.TempDir()
	cases := []struct {
		name  string
		want  string // substring of the Check error; "" = must be valid
		apply func(r *Report)
	}{
		{"new fields valid", "", func(r *Report) {
			r.Summary.Iterations, r.Summary.DroppedIterations, r.Summary.ToolErrors = I64(10), I64(0), I64(0)
			r.Run.Generator = &Generator{Cores: 4, CPUAvgPct: F(12.5), CPUMaxPct: nil}
			r.Verdicts = append(r.Verdicts, Verdict{ID: VerdictGenerator, Status: StatusPass, Signal: "summary.droppedIterations", Message: "ok"})
		}},
		{"offset without colon valid", "", func(r *Report) { r.Run.StartedAt = "2026-09-29T15:00:00+0200" }},
		{"bad verdict id", "verdicts[0].id", func(r *Report) { r.Verdicts[0].ID = "memory" }},
		{"bad status", "status invalid", func(r *Report) { r.Verdicts[0].Status = "ok" }},
		{"relative url", "target.url", func(r *Report) { r.Run.Target.URL = "/mcp" }},
		{"url with space", "target.url", func(r *Report) { r.Run.Target.URL = "http://local host/mcp" }},
		{"bad time", "startedAt", func(r *Report) { r.Run.StartedAt = "2026-09-29 13:00:00" }},
		{"time without zone", "endedAt", func(r *Report) { r.Run.EndedAt = "2026-09-29T13:40:00" }},
		{"bad byErrorType key", "byErrorType key", func(r *Report) {
			r.Summary.ByErrorType["Bad-Key"] = 1
			r.Summary.Errors++
			r.Summary.Reqs++
		}},
		{"empty tool name", "tool.name", func(r *Report) { r.Tool.Name = "" }},
		{"empty scenario", "run.scenario", func(r *Report) { r.Run.Scenario = "" }},
		{"bad git sha", "git.sha", func(r *Report) { r.Run.Git = &Git{SHA: "XYZ"} }},
		{"negative duration", "durationS", func(r *Report) { r.Run.DurationS = -1 }},
		{"rate > 1", "errorRate", func(r *Report) { r.Tools[0].ErrorRate = 1.5 }},
		{"generator pct > 100", "cpuAvgPct", func(r *Report) { r.Run.Generator = &Generator{Cores: 2, CPUAvgPct: F(140)} }},
		{"generator cores 0", "cores", func(r *Report) { r.Run.Generator = &Generator{Cores: 0} }},
		{"zero arrival time unit", "arrivalTimeUnitS", func(r *Report) { r.Run.Load.ArrivalTimeUnitS = F(0) }},
		{"negative t", "series.t", func(r *Report) { r.Series.T[0] = -30 }},
		{"tool series length", "tools[search].p95Ms", func(r *Report) {
			r.Series.Tools["search"] = ToolSeries{P95Ms: r.Series.Tools["search"].P95Ms[1:]}
		}},
		{"dropped series length", "client.droppedIterations", func(r *Report) { r.Series.Client.DroppedIterations = r.Series.Client.DroppedIterations[2:] }},
		{"negative iterations", "summary.iterations", func(r *Report) { r.Summary.Iterations = I64(-1) }},
		{"capacity valid", "", func(r *Report) { r.Capacity = sampleCapacity() }},
		{"capacity steps unsorted", "strictly increasing vus", func(r *Report) {
			r.Capacity = sampleCapacity()
			r.Capacity.Steps[1].VUs = 5
		}},
		{"capacity step errors > reqs", "errors must be in [0, reqs]", func(r *Report) {
			r.Capacity = sampleCapacity()
			r.Capacity.Steps[0].Errors = 1000
		}},
		{"capacity tool percentiles", "p95<=p99", func(r *Report) {
			r.Capacity = sampleCapacity()
			r.Capacity.Steps[1].Tools[0].P99 = F(1)
		}},
		{"capacity bad breached", "breached has an invalid entry", func(r *Report) {
			r.Capacity = sampleCapacity()
			r.Capacity.Steps[1].Tools[0].Breached = []string{"latency"}
		}},
		{"capacity breaking 0", "capacity.breakingVus", func(r *Report) {
			r.Capacity = sampleCapacity()
			r.Capacity.BreakingVUs = I(0)
		}},
		{"resilience blocks valid", "", func(r *Report) {
			r.Sessions, r.Chaos, r.CallIntegrity = sampleSessions(), sampleChaos(), sampleIntegrity()
			r.Verdicts = append(r.Verdicts,
				Verdict{ID: VerdictSessionSurvival, Status: StatusFail, Signal: "mcp_session_lifetime", Message: "x"},
				Verdict{ID: VerdictRecovery, Status: StatusPass, Signal: "chaos.recovery", Message: "x"},
				Verdict{ID: VerdictCallIntegrity, Status: StatusWarn, Signal: "callIntegrity", Message: "x"})
		}},
		{"chaos not run valid", "", func(r *Report) { r.Chaos = &Chaos{Action: "restart", Container: "c"} }},
		{"sessions do not add up", "sessions.survived + sessions.died", func(r *Report) {
			r.Sessions = sampleSessions()
			r.Sessions.Survived++
		}},
		{"sessions bad cause key", "diedByCause", func(r *Report) {
			r.Sessions = sampleSessions()
			r.Sessions.DiedByCause = map[string]int64{"Not Found": 2}
		}},
		{"chaos bad action", "chaos.action", func(r *Report) {
			r.Chaos = sampleChaos()
			r.Chaos.Action = "kill"
		}},
		{"chaos recovered without time", "recoveryS must be set exactly when recovered", func(r *Report) {
			r.Chaos = sampleChaos()
			r.Chaos.Recovery.RecoveryS = nil
		}},
		{"chaos failures > attempts", "connectFailures <= connectAttempts", func(r *Report) {
			r.Chaos = sampleChaos()
			r.Chaos.Recovery.ConnectFailures = 100
		}},
		{"integrity does not add up", "failedButExecuted + neverRan", func(r *Report) {
			r.CallIntegrity = sampleIntegrity()
			r.CallIntegrity.NeverRan++
		}},
		{"integrity negative", "callIntegrity.tagged", func(r *Report) {
			r.CallIntegrity = sampleIntegrity()
			r.CallIntegrity.Tagged = -1
		}},
		{"capacity byErrorType valid", "", func(r *Report) {
			r.Capacity = sampleCapacity()
			r.Capacity.Steps[1].ByErrorType = map[string]int64{"timeout": 4, "http": 1}
			r.Capacity.Steps[1].Tools[0].ByErrorType = map[string]int64{"timeout": 4}
		}},
		{"capacity bad byErrorType key", "byErrorType key", func(r *Report) {
			r.Capacity = sampleCapacity()
			r.Capacity.Steps[1].Tools[0].ByErrorType = map[string]int64{"Time-Out": 4}
		}},
		{"cancellation valid", "", func(r *Report) {
			r.Cancellation = sampleCancellation()
			r.Verdicts = append(r.Verdicts, Verdict{ID: VerdictCancellation, Status: StatusWarn, Signal: "mcp_cancellations", Message: "x"})
		}},
		{"cancellation unmeasured valid", "", func(r *Report) {
			r.Cancellation = sampleCancellation()
			r.Cancellation.Server = nil
		}},
		{"comparison valid", "", func(r *Report) {
			r.Comparison = sampleComparison()
			r.Verdicts = append(r.Verdicts, Verdict{ID: VerdictRegression, Status: StatusFail, Signal: "comparison", Message: "x"})
		}},
		{"comparison not regressed valid", "", func(r *Report) {
			r.Comparison = sampleComparison()
			r.Comparison.Tools[0].Status, r.Comparison.Tools[0].Regressed, r.Comparison.Regressed = DeltaOK, nil, false
		}},
		{"comparison regressed flag", "comparison.regressed must be true exactly", func(r *Report) {
			r.Comparison = sampleComparison()
			r.Comparison.Regressed = false
		}},
		{"comparison added with base", "an added tool has current and no base", func(r *Report) {
			r.Comparison = sampleComparison()
			r.Comparison.Tools[1].Base = r.Comparison.Tools[0].Base
		}},
		{"comparison bad tool status", "status invalid", func(r *Report) {
			r.Comparison = sampleComparison()
			r.Comparison.Tools[2].Status = "worse"
		}},
		{"comparison tool percentiles", "p50<=p95<=p99", func(r *Report) {
			r.Comparison = sampleComparison()
			r.Comparison.Tools[0].Current.P99 = 1
		}},
		{"comparison bad unit", "unit invalid", func(r *Report) {
			r.Comparison = sampleComparison()
			r.Comparison.Metrics[0].Unit = "%"
		}},
		{"comparison bad git sha", "comparison.baseline.git.sha", func(r *Report) {
			r.Comparison = sampleComparison()
			r.Comparison.Baseline.Git = &Git{SHA: "main"}
		}},
		{"cancellation late > cancels", "lateResponses", func(r *Report) {
			r.Cancellation = sampleCancellation()
			r.Cancellation.LateResponses = 99
		}},
		{"cancellation server percentiles", "p50<=p95", func(r *Report) {
			r.Cancellation = sampleCancellation()
			r.Cancellation.Server.WorkAfterCancelP95Ms = F(1)
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := ReadJSON(examplePath("leaky.json"))
			if err != nil {
				t.Fatal(err)
			}
			c.apply(r)
			err = r.Check()
			if c.want == "" && err != nil {
				t.Fatalf("Check rejected a valid report: %v", err)
			}
			if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("Check: want error containing %q, got %v", c.want, err)
			}
			if validate == nil {
				return
			}
			path := filepath.Join(dir, fmt.Sprintf("case%d.json", i))
			b, _ := Marshal(r)
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
			ok, out := validate(path)
			if ok != (c.want == "") {
				t.Fatalf("validate.mjs disagrees with Check (valid=%v):\n%s", ok, out)
			}
		})
	}
}

func sampleCancellation() *Cancellation {
	return &Cancellation{Cancels: 10, ByOutcome: map[string]int64{"cancelled": 2, "late_response": 8, "completed": 3}, ByReason: map[string]int64{"client": 10},
		ByTool: map[string]int64{"slow": 10}, LateResponses: 8, SendMs: &Latency{Count: 10, P50: 1, P95: 2, P99: 2, Max: 3},
		Server: &CancelServer{Cancelled: 10, Observed: 10, WorkAfterCancelP50Ms: F(1800), WorkAfterCancelP95Ms: F(2400), WorkAfterCancelTotalS: 18}}
}

func sampleComparison() *Comparison {
	s := func(p95 float64) *ToolSample {
		return &ToolSample{Reqs: 1000, Errors: 1, ErrorRate: 0.001, P50: p95 / 2, P95: p95, P99: 400, RPS: 16.7}
	}
	return &Comparison{
		Baseline: BaselineRef{Source: "baseline/report.json", RunID: "r1", StartedAt: "2026-09-29T13:00:00Z", Scenario: "agent-session", Protocol: "2025-11-25",
			Git: &Git{SHA: "4a1b2c3d", Ref: "refs/heads/main"}},
		Rules:     CompareRules{MaxP95Increase: 0.2, MaxP99Increase: 0.3, MaxErrorIncrease: 0.5, MinErrorDelta: 0.005, MinDeltaMs: 25, MinCalls: 50, MaxLeakSlopeIncrease: 1},
		Regressed: true,
		Reasons:   []string{"`search` p95 210 ms → 284 ms (+35%, +74 ms)"},
		Warnings:  []string{},
		Tools: []ToolDelta{
			{Name: "search", Status: DeltaRegressed, Base: s(210), Current: s(284), Regressed: []string{"p95"}},
			{Name: "new", Status: DeltaAdded, Current: s(20)},
			{Name: "old", Status: DeltaRemoved, Base: s(20)},
		},
		Metrics: []MetricDelta{
			{ID: "errorRate", Label: "error rate", Unit: "rate", Base: F(0.001), Current: F(0.0012), Status: DeltaOK},
			{ID: "memoryGrowthMiB", Label: "memory growth", Unit: "MiB", Base: F(0.2), Current: F(4.8), Status: DeltaNA, Note: "not judged"},
		},
	}
}

func sampleCapacity() *Capacity {
	return &Capacity{
		PlannedVUs: []int{10, 20, 40}, MinAgents: I(10), MaxSustainableVUs: I(10), BreakingVUs: I(20),
		Budgets: CapacityBudgets{ConnectP95Ms: 1500, ConnectErrorRate: 0.01, Tools: map[string]ToolBudget{"slow": {P95Ms: 800, P99Ms: 2000, ErrorRate: 0.01}}},
		Steps: []Step{
			{VUs: 10, StartS: 5, EndS: 65, Reqs: 600, Errors: 2, ErrorRate: 0.0033, RPS: 10, ConnectP95Ms: F(8), ConnectErrorRate: F(0), GeneratorCPUMaxPct: F(4), Passed: true, Breaches: []string{},
				Tools: []StepTool{{Name: "slow", Reqs: 100, ErrorRate: 0, P95: F(320), P99: F(400)}}},
			{VUs: 20, StartS: 70, EndS: 130, Reqs: 900, Errors: 5, ErrorRate: 0.0056, RPS: 15, ConnectP95Ms: nil, ConnectErrorRate: nil, Breached: []string{"connectP95Ms"},
				Breaches: []string{"`slow` p95 1.9 s > 800 ms"}, Tools: []StepTool{{Name: "slow", Reqs: 100, P95: F(1900), P99: F(2100), Breached: []string{"p95", "p99"}}}},
		},
	}
}

func sampleSessions() *Sessions {
	return &Sessions{Total: 10, Survived: 8, Died: 2, DiedByCause: map[string]int64{"session_not_found": 2}, DiedAfterS: F(302.5), LifetimeP50S: 600,
		Reconnects: 2, DriftTool: "search", EarlyP95Ms: F(40), LateP95Ms: F(44)}
}

func sampleChaos() *Chaos {
	return &Chaos{Action: "restart", Container: "mcpload-chaos-ts", Ran: true, AtS: 30.2, DurationS: 0.8, Recovery: &Recovery{
		Recovered: true, RecoveryS: F(2), ServerBackS: F(1.3), LastReconnectS: F(2), BudgetS: 30, WindowS: 5, ErrorRate: 0.01, ConnectP95Ms: 1500,
		Reconnects: 10, ConnectAttempts: 42, ConnectFailures: 32, ErrorsByType: map[string]int64{"http": 57, "session_not_found": 3}}}
}

func sampleIntegrity() *CallIntegrity {
	return &CallIntegrity{Source: "http://localhost:3019/calls", Tagged: 2280, Retried: 26, ClientFailed: 3, Executed: 2280, Executions: 2285,
		FailedButExecuted: 2, NeverRan: 1, Duplicated: 5, DuplicateExecutions: 5, DuplicatedAfterRetry: 5}
}

// A '$' in the label must be copied literally into <title> by both renderers.
func TestRenderTitleDollar(t *testing.T) {
	r, err := ReadJSON(examplePath("healthy.json"))
	if err != nil {
		t.Fatal(err)
	}
	r.Run.Target.Label = "cost $$ $1 $' $&x api"
	want := "<title>mcpload · cost $$ $1 $' $x api · soak</title>"
	var buf bytes.Buffer
	if err := RenderHTML(r, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("Go title: %s", regexp.MustCompile(`<title>.*</title>`).FindString(buf.String()))
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found")
	}
	dir := t.TempDir()
	in, out := filepath.Join(dir, "r.json"), filepath.Join(dir, "r.html")
	if err := WriteJSON(in, r); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command(node, filepath.Join(repoRoot, "report", "render.mjs"), in, out, "--no-validate").CombinedOutput(); err != nil {
		t.Fatalf("render.mjs: %v\n%s", err, b)
	}
	html, _ := os.ReadFile(out)
	if !strings.Contains(string(html), want) {
		t.Fatalf("render.mjs title: %s", regexp.MustCompile(`<title>.*</title>`).FindString(string(html)))
	}
}
