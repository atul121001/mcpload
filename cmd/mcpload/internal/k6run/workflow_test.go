package k6run

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWorkflowStats(t *testing.T) {
	pt := func(metric string, v float64, tags string) string {
		return `{"metric":"` + metric + `","type":"Point","data":{"time":"2026-10-03T00:00:01Z","value":` +
			strconv.FormatFloat(v, 'f', -1, 64) + `,"tags":{` + tags + `}}}`
	}
	step := func(name string, v float64) string { return pt(MetricWorkflowStepDuration, v, `"step":"`+name+`"`) }
	lines := []string{
		// plan order is first-seen order, not alphabetical
		step("gather", 10), step("inspect", 200), step("gather", 30), step("act", 50), step("inspect", 400),
		pt(MetricWorkflowDuration, 1200, ""), pt(MetricWorkflowDuration, 1800, ""),
		pt(MetricWorkflowComplete, 1, ""), pt(MetricWorkflowComplete, 1, ""), pt(MetricWorkflowComplete, 0, ""),
	}
	origin, _ := time.Parse(time.RFC3339, "2026-10-03T00:00:00Z")
	a := NewAggregator(origin, 10*time.Second, nil)
	if err := a.Read(strings.NewReader(strings.Join(lines, "\n"))); err != nil {
		t.Fatal(err)
	}
	w := a.Workflow()
	if w == nil {
		t.Fatal("Workflow() = nil")
	}
	if w.Runs != 3 || w.Completed != 2 {
		t.Errorf("runs/completed = %d/%d, want 3/2", w.Runs, w.Completed)
	}
	if w.Duration.Count != 2 || w.Duration.P50 != 1500 || w.Duration.Max != 1800 {
		t.Errorf("duration = %+v", w.Duration)
	}
	var names []string
	for _, s := range w.Steps {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "gather,inspect,act" {
		t.Errorf("steps = %v, want gather,inspect,act", names)
	}
	if g := w.Steps[0]; g.Count != 2 || g.P50 != 20 || g.Max != 30 || !(g.P95 <= g.P99 && g.P99 <= g.Max) {
		t.Errorf("gather = %+v", g)
	}
}

func TestWorkloadStats(t *testing.T) {
	pt := func(metric string, v float64, tags string) string {
		return `{"metric":"` + metric + `","type":"Point","data":{"time":"2026-10-03T00:00:01Z","value":` +
			strconv.FormatFloat(v, 'f', -1, 64) + `,"tags":{` + tags + `}}}`
	}
	flow := func(f string) string { return `"flow":"` + f + `"` }
	step := func(f, s string, v float64) string {
		return pt(MetricWorkloadStepDuration, v, flow(f)+`,"flow_step":"`+s+`"`)
	}
	lines := []string{
		step("orders", "search_customer", 10), step("ticket", "search_customer", 12), step("orders", "get_orders", 40),
		step("ticket", "create_ticket", 400), step("orders", "search_customer", 30),
		pt(MetricWorkloadFlowDuration, 900, flow("orders")), pt(MetricWorkloadFlowDuration, 1100, flow("orders")),
		pt(MetricWorkloadFlowComplete, 1, flow("orders")), pt(MetricWorkloadFlowComplete, 1, flow("orders")),
		pt(MetricWorkloadFlowComplete, 0, flow("ticket")),
		// a tools/call tagged with the flow still counts for its tool
		pt(MetricReqs, 1, `"method":"tools/call","tool":"search",`+flow("orders")),
	}
	origin, _ := time.Parse(time.RFC3339, "2026-10-03T00:00:00Z")
	a := NewAggregator(origin, 10*time.Second, nil)
	if err := a.Read(strings.NewReader(strings.Join(lines, "\n"))); err != nil {
		t.Fatal(err)
	}
	fs := a.Workload()
	if len(fs) != 2 || fs[0].Name != "orders" || fs[1].Name != "ticket" {
		t.Fatalf("flows = %+v", fs)
	}
	o, tk := fs[0], fs[1]
	if o.Runs != 2 || o.Completed != 2 || o.Duration.Count != 2 || o.Duration.P50 != 1000 {
		t.Errorf("orders = %+v", o)
	}
	if len(o.Steps) != 2 || o.Steps[0].Name != "search_customer" || o.Steps[0].Count != 2 || o.Steps[1].Name != "get_orders" {
		t.Errorf("orders steps = %+v", o.Steps)
	}
	if tk.Runs != 1 || tk.Completed != 0 || tk.Duration.Count != 0 || len(tk.Steps) != 2 {
		t.Errorf("ticket = %+v", tk)
	}
	// flow_step, not step: nothing lands in the workflow or step-load aggregates
	if a.Workflow() != nil || len(a.Steps()) != 0 {
		t.Error("workload metrics leaked into workflow or step-load aggregates")
	}
	if ts := a.Tools(); len(ts) != 1 || ts[0].Reqs != 1 {
		t.Errorf("tools = %+v", ts)
	}
	if parseDropped(t).Workload() != nil {
		t.Error("Workload() != nil for a run without workload metrics")
	}
}

func TestWorkflowStatsAbsent(t *testing.T) {
	a := parseDropped(t)
	if w := a.Workflow(); w != nil {
		t.Errorf("Workflow() = %+v, want nil for a run without workflow metrics", w)
	}
}
