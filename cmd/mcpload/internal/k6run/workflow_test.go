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

func TestWorkflowStatsAbsent(t *testing.T) {
	a := parseDropped(t)
	if w := a.Workflow(); w != nil {
		t.Errorf("Workflow() = %+v, want nil for a run without workflow metrics", w)
	}
}
