package analysis

import (
	"encoding/json"
	"flag"
	"path/filepath"
	"testing"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

var update = flag.Bool("update", false, "recompute verdicts of report/examples/*.json with this package and rewrite the files")

// exampleVerdicts computes the verdicts exactly as `mcpload run` does, and
// the capacity block's derived fields (estimate, degradation, failure).
func exampleVerdicts(r *report.Report) ([]report.Verdict, *report.Capacity) {
	var cp *report.Capacity
	vs := Verdicts(r, DefaultConfig())
	vs = append(vs,
		SessionNotFoundVerdict(r.Run.Protocol, float64(r.Summary.ByErrorType["session_not_found"]), float64(r.Summary.Reqs)),
		ThresholdVerdict(r.Thresholds),
		GeneratorVerdict(r))
	if c := r.Capacity; c != nil {
		// Every tool of the run has its budget in the report; the default only
		// supplies the connect error budget.
		cfg := CapacityConfig{Default: report.ToolBudget{ErrorRate: c.Budgets.ConnectErrorRate}, Tools: c.Budgets.Tools,
			ConnectP95: c.Budgets.ConnectP95Ms, Planned: c.PlannedVUs, StoppedEarly: c.StoppedEarly}
		if c.MinAgents != nil {
			cfg.MinAgents = *c.MinAgents
		}
		var v report.Verdict
		cp, v = CapacityVerdict(append([]report.Step(nil), c.Steps...), cfg)
		cp.MinAgents = c.MinAgents
		vs = append(SkipDriftForSteps(vs), v)
	}
	return vs, cp
}

// TestExamples keeps the verdicts in report/examples/*.json equal to what the
// CLI computes for their series. report/examples/generate.mjs writes the data
// of healthy and leaky (step-load.json is a real short run against the
// ts-pooled demo server); refresh the verdicts with
//
//	go test ./internal/analysis -run TestExamples -update
//
// then re-render the HTML with node report/render.mjs.
func TestExamples(t *testing.T) {
	for _, name := range []string{"healthy", "leaky", "step-load"} {
		path := filepath.Join("..", "..", "..", "..", "report", "examples", name+".json")
		r, err := report.ReadJSON(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, cp := exampleVerdicts(r)
		if *update {
			r.Verdicts = got
			if cp != nil {
				r.Capacity = cp
			}
			if err := r.Check(); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if err := report.WriteJSON(path, r); err != nil {
				t.Fatal(err)
			}
			t.Logf("rewrote %s", path)
			continue
		}
		a, _ := json.MarshalIndent(got, "", "  ")
		b, _ := json.MarshalIndent(r.Verdicts, "", "  ")
		if string(a) != string(b) {
			t.Errorf("%s: verdicts differ from the CLI's; run `go test ./internal/analysis -run TestExamples -update`\ncli:\n%s\nexample:\n%s", name, a, b)
		}
		if cp != nil {
			a, _ = json.MarshalIndent(cp, "", "  ")
			b, _ = json.MarshalIndent(r.Capacity, "", "  ")
			if string(a) != string(b) {
				t.Errorf("%s: capacity differs from the CLI's; run `go test ./internal/analysis -run TestExamples -update`\ncli:\n%s\nexample:\n%s", name, a, b)
			}
		}
	}
}
