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
