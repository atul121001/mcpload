package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/analysis"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/k6run"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/workload"
)

// workloadScenario is the bundled scenario that runs a workload profile
// (scenarios/workload.js; also its options.tags.scenario_name).
const workloadScenario = "workload"

// workloadFileEnv names the normalized workload JSON for scenarios/workload.js.
const workloadFileEnv = "WORKLOAD_FILE"

// writeWorkloadFile saves a loaded workload as the normalized JSON the
// scenario reads (WORKLOAD_FILE) in a temporary file, returning its absolute
// path and a cleanup func. Any command that runs scenarios/workload.js (run,
// capacity) passes the path to k6 as WORKLOAD_FILE.
func writeWorkloadFile(w *workload.Workload) (string, func(), error) {
	f, err := os.CreateTemp("", "mcpload-workload-*.json")
	if err != nil {
		return "", nil, err
	}
	path := f.Name()
	f.Close()
	cleanup := func() { os.Remove(path) }
	if err := workload.Write(path, w); err != nil {
		cleanup()
		return "", nil, err
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return path, cleanup, nil
}

// workloadReport merges the profile (flow order, weights, budgets) with the
// per-flow metrics into report.workload. Flows that never ran are kept with
// zero runs, so a low-weight flow that a short run missed is visible.
func workloadReport(def *workload.Workload, stats []k6run.FlowStats) *report.Workload {
	byName := map[string]k6run.FlowStats{}
	for _, s := range stats {
		byName[s.Name] = s
	}
	out := &report.Workload{Name: def.Name, Description: def.Description, Flows: []report.WorkloadFlow{}}
	for _, f := range def.Flows {
		s := byName[f.Name]
		fl := report.WorkloadFlow{Name: f.Name, Weight: f.Weight, Runs: s.Runs, Completed: s.Completed,
			CompletionRate: round6(ratio(s.Completed, s.Runs)), DurationMs: toLatency(s.Duration), Steps: []report.WorkloadStep{},
			Budget: &report.Budget{P95Ms: report.F(f.Budgets.P95), P99Ms: f.Budgets.P99, MinCompletionRate: report.F(f.Budgets.Completion)}}
		seen := map[string]k6run.WorkflowStep{}
		for _, st := range s.Steps {
			seen[st.Name] = st
		}
		// Plan order; a step that never ran (the flow always stopped earlier) has count 0.
		for _, st := range f.Steps {
			ws := report.WorkloadStep{Name: st.Name, Latency: toLatency(seen[st.Name].Latency)}
			if b, ok := f.Budgets.Steps[st.Name]; ok {
				ws.Budget = &report.Budget{P95Ms: b.P95, P99Ms: b.P99}
			}
			fl.Steps = append(fl.Steps, ws)
		}
		out.Flows = append(out.Flows, fl)
	}
	return out
}

// printWorkload prints the per-flow table.
func printWorkload(w io.Writer, wl *report.Workload) {
	if wl == nil || len(wl.Flows) == 0 {
		return
	}
	var total float64
	var runs int64
	width := len("flow")
	for _, f := range wl.Flows {
		total += f.Weight
		runs += f.Runs
		width = max(width, len(f.Name))
	}
	fmt.Fprintf(w, "\nmcpload workload %s (%d flows, %d runs; end-to-end times of completed flows):\n", wl.Name, len(wl.Flows), runs)
	fmt.Fprintf(w, "  %-*s  %6s  %6s  %9s  %8s  %8s  %8s  %s\n", width, "flow", "weight", "runs", "completed", "p50", "p95", "p99", "slowest step (p95)")
	for _, f := range wl.Flows {
		share := 0.0
		if total > 0 {
			share = 100 * f.Weight / total
		}
		done, p50, p95, p99, slow := "-", "-", "-", "-", "-"
		if f.Runs > 0 {
			done = fmt.Sprintf("%.1f%%", 100*f.CompletionRate)
		}
		if f.DurationMs.Count > 0 {
			p50, p95, p99 = analysis.FormatMs(f.DurationMs.P50), analysis.FormatMs(f.DurationMs.P95), analysis.FormatMs(f.DurationMs.P99)
		}
		var worst *report.WorkloadStep
		for i, st := range f.Steps {
			if st.Count > 0 && (worst == nil || st.P95 > worst.P95) {
				worst = &f.Steps[i]
			}
		}
		if worst != nil {
			slow = worst.Name + " " + analysis.FormatMs(worst.P95)
		}
		fmt.Fprintf(w, "  %-*s  %5.0f%%  %6d  %9s  %8s  %8s  %8s  %s\n", width, f.Name, share, f.Runs, done, p50, p95, p99, slow)
	}
}

// workloadDef is the profile behind a run: the one --workload loaded, else
// the WORKLOAD_FILE the script was given (--env), else nil.
func workloadDef(o *runOpts, envMap map[string]string, logf func(string, ...any)) *workload.Workload {
	if o.wl != nil {
		return o.wl
	}
	p := strings.TrimSpace(envMap[workloadFileEnv])
	if p == "" {
		return nil
	}
	w, err := workload.ReadNormalized(p)
	if err != nil {
		logf("warning: %s: %v (no workload table in the report)", workloadFileEnv, err)
		return nil
	}
	return w
}
