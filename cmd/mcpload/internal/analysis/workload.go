package analysis

import (
	"fmt"
	"math"
	"strings"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

// Workload profile (scenario workload, report/schema/README.md "Verdict ids").
//
// Each flow of the profile is judged against the budget it ran with
// (report.workload.flows[].budget, and each step's budget):
//
//   - fail when a flow that ran has an end-to-end p95 or p99 over its budget,
//     a completion rate under its minimum, or a step over its step budget.
//     The message names every failing flow in plain words, e.g.
//     "flow `create-ticket` p95 4.1 s > 3 s budget; 92% completed (< 99%)".
//   - warn when every flow that ran held its budgets but some flow never ran
//     (a low weight in a short run): it was not tested.
//   - fail when no flow ran at all; skipped without workload metrics.
//
// Latencies come from the report (the same numbers as the flow table); the
// k6 thresholds on the same budgets are judged separately (verdict threshold).

// WorkloadVerdict judges every flow of report.workload against its budgets.
func WorkloadVerdict(w *report.Workload) report.Verdict {
	const id, signal = report.VerdictWorkload, "workload.flows"
	if w == nil || len(w.Flows) == 0 {
		return skipped(id, signal, "the run emitted no workload metrics (scenario workload, mcpload run --workload)")
	}
	var total float64
	var runs int64
	for _, f := range w.Flows {
		total += f.Weight
		runs += f.Runs
	}
	if runs == 0 {
		return report.Verdict{ID: id, Status: report.StatusFail, Signal: signal,
			Message: fmt.Sprintf("No flow of `%s` ran: check the connect errors and the k6 output.", w.Name)}
	}

	var failed, idle []string
	closest, closestRatio := "", -1.0
	for _, f := range w.Flows {
		if f.Runs == 0 {
			idle = append(idle, fmt.Sprintf("`%s` (weight %s)", f.Name, weightShare(f.Weight, total)))
			continue
		}
		var why []string
		if b := f.Budget; b != nil {
			if f.DurationMs.Count > 0 {
				why = appendLatency(why, "p95", f.DurationMs.P95, b.P95Ms)
				why = appendLatency(why, "p99", f.DurationMs.P99, b.P99Ms)
				if b.P95Ms != nil {
					if r := ratioOf(f.DurationMs.P95, *b.P95Ms); r > closestRatio {
						closestRatio = r
						closest = fmt.Sprintf("`%s` p95 %s of %s", f.Name, FormatMs(f.DurationMs.P95), FormatMs(*b.P95Ms))
					}
				}
			}
			if m := b.MinCompletionRate; m != nil && f.CompletionRate < *m {
				why = append(why, fmt.Sprintf("%s completed (< %s)", fmtPct(f.CompletionRate), fmtPct(*m)))
			}
		}
		for _, st := range f.Steps {
			if st.Budget == nil || st.Count == 0 {
				continue
			}
			var sw []string
			sw = appendLatency(sw, "p95", st.P95, st.Budget.P95Ms)
			sw = appendLatency(sw, "p99", st.P99, st.Budget.P99Ms)
			for _, s := range sw {
				why = append(why, fmt.Sprintf("step `%s` %s", st.Name, s))
			}
		}
		if len(why) > 0 {
			failed = append(failed, fmt.Sprintf("flow `%s` %s", f.Name, strings.Join(why, "; ")))
		}
	}
	if len(failed) > 0 {
		return report.Verdict{ID: id, Status: report.StatusFail, Signal: signal, Message: strings.Join(failed, ". ") + "."}
	}
	held := len(w.Flows) - len(idle)
	msg := fmt.Sprintf("%d of %d flows of `%s` held their budgets", held, len(w.Flows), w.Name)
	if held == len(w.Flows) {
		msg = fmt.Sprintf("All %d flows of `%s` held their budgets", held, w.Name)
	}
	if closest != "" {
		msg += " (closest: " + closest + ")"
	}
	if len(idle) > 0 {
		return report.Verdict{ID: id, Status: report.StatusWarn, Signal: signal,
			Message: msg + fmt.Sprintf(". Never ran, so not tested: %s; run longer or with more agents.", strings.Join(idle, ", "))}
	}
	return report.Verdict{ID: id, Status: report.StatusPass, Signal: signal, Message: msg + "."}
}

// appendLatency adds "p95 4.1 s > 3 s budget" when v is over the budget.
func appendLatency(why []string, label string, v float64, budget *float64) []string {
	if budget == nil || v <= *budget {
		return why
	}
	return append(why, fmt.Sprintf("%s %s > %s budget", label, FormatMs(v), FormatMs(*budget)))
}

// weightShare is a flow's share of the total weight, e.g. "10%".
func weightShare(w, total float64) string {
	if total <= 0 {
		return "0%"
	}
	return fmtPct(math.Round(w/total*1e4) / 1e4)
}
