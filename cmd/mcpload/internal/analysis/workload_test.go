package analysis

import (
	"strings"
	"testing"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

func sampleWorkload() *report.Workload {
	lat := func(n int64, p50, p95, p99 float64) report.Latency {
		return report.Latency{Count: n, P50: p50, P95: p95, P99: p99, Max: p99 + 10}
	}
	return &report.Workload{Name: "customer-support", Flows: []report.WorkloadFlow{
		{Name: "lookup-orders", Weight: 60, Runs: 600, Completed: 600, CompletionRate: 1, DurationMs: lat(600, 500, 1400, 1800),
			Budget: &report.Budget{P95Ms: report.F(2000), MinCompletionRate: report.F(0.99)},
			Steps:  []report.WorkloadStep{{Name: "get_orders", Latency: lat(600, 5, 9, 12), Budget: &report.Budget{P95Ms: report.F(800)}}}},
		{Name: "check-subscription", Weight: 30, Runs: 300, Completed: 300, CompletionRate: 1, DurationMs: lat(300, 200, 900, 1300),
			Budget: &report.Budget{P95Ms: report.F(2000), MinCompletionRate: report.F(0.99)}, Steps: []report.WorkloadStep{}},
		{Name: "create-ticket", Weight: 10, Runs: 100, Completed: 100, CompletionRate: 1, DurationMs: lat(100, 1000, 1850, 2300),
			Budget: &report.Budget{P95Ms: report.F(2500), P99Ms: report.F(5000), MinCompletionRate: report.F(0.95)}, Steps: []report.WorkloadStep{}},
	}}
}

func TestWorkloadVerdictPass(t *testing.T) {
	v := WorkloadVerdict(sampleWorkload())
	if v.Status != report.StatusPass || v.ID != report.VerdictWorkload {
		t.Fatalf("%s %s: %s", v.ID, v.Status, v.Message)
	}
	if want := "All 3 flows of `customer-support` held their budgets (closest: `create-ticket` p95 1.85 s of 2.5 s)."; v.Message != want {
		t.Errorf("message = %q\nwant      %q", v.Message, want)
	}
}

func TestWorkloadVerdictNamesFailingFlows(t *testing.T) {
	w := sampleWorkload()
	ct := &w.Flows[2]
	ct.DurationMs.P95, ct.DurationMs.P99, ct.DurationMs.Max = 4100, 4500, 4600
	ct.Completed, ct.CompletionRate = 92, 0.92
	ct.Budget.MinCompletionRate = report.F(0.99)
	w.Flows[0].Steps[0].P95, w.Flows[0].Steps[0].P99, w.Flows[0].Steps[0].Max = 950, 990, 1000
	v := WorkloadVerdict(w)
	if v.Status != report.StatusFail {
		t.Fatalf("status %s: %s", v.Status, v.Message)
	}
	want := "flow `lookup-orders` step `get_orders` p95 950 ms > 800 ms budget. flow `create-ticket` p95 4.1 s > 2.5 s budget; 92% completed (< 99%)."
	if v.Message != want {
		t.Errorf("message = %q\nwant      %q", v.Message, want)
	}
}

func TestWorkloadVerdictIdleFlowWarns(t *testing.T) {
	w := sampleWorkload()
	w.Flows[2] = report.WorkloadFlow{Name: "create-ticket", Weight: 10, Budget: w.Flows[2].Budget, Steps: []report.WorkloadStep{}}
	v := WorkloadVerdict(w)
	if v.Status != report.StatusWarn || !strings.Contains(v.Message, "2 of 3 flows") || !strings.Contains(v.Message, "Never ran, so not tested: `create-ticket` (weight 10%)") {
		t.Errorf("%s: %s", v.Status, v.Message)
	}
}

func TestWorkloadVerdictNothingRan(t *testing.T) {
	if v := WorkloadVerdict(nil); v.Status != report.StatusSkipped {
		t.Errorf("nil: %s", v.Status)
	}
	w := sampleWorkload()
	for i := range w.Flows {
		w.Flows[i] = report.WorkloadFlow{Name: w.Flows[i].Name, Weight: 1, Steps: []report.WorkloadStep{}}
	}
	if v := WorkloadVerdict(w); v.Status != report.StatusFail || !strings.Contains(v.Message, "No flow of `customer-support` ran") {
		t.Errorf("%s: %s", v.Status, v.Message)
	}
	// Every run failed: 0% completed, no durations.
	w = sampleWorkload()
	w.Flows[1].Completed, w.Flows[1].CompletionRate, w.Flows[1].DurationMs = 0, 0, report.Latency{}
	if v := WorkloadVerdict(w); v.Status != report.StatusFail || !strings.Contains(v.Message, "flow `check-subscription` 0% completed (< 99%)") {
		t.Errorf("%s: %s", v.Status, v.Message)
	}
}
