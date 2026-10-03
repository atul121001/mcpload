package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/analysis"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/k6run"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/sampler"
)

// stepLoadScenario is the scenario_name tag of scenarios/step-load.js and
// stepLoadK6Scenario its k6 scenario (whose stages are the planned steps).
const (
	stepLoadScenario   = "step-load"
	stepLoadK6Scenario = "steps"
)

// capacityConfig mirrors the budgets of scenarios/lib/config.js, so each step
// is judged as k6 would judge the thresholds the other scenarios set:
// P95_MS (800), P99_MS (2000), ERR_RATE (0.01), CONNECT_P95_MS (1500), and
// TOOL_BUDGETS shallow-merged over {"flaky":{"errRate":0.2}} (an entry
// replaces the built-in one for its tool; missing fields fall back to the
// defaults). MIN_AGENTS (0 = unset) is the target concurrency.
func capacityConfig(env map[string]string) (analysis.CapacityConfig, error) {
	num := func(k string, def float64) (float64, error) {
		v := strings.TrimSpace(env[k])
		if v == "" {
			return def, nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, fmt.Errorf("%s must be a number, got %q", k, v)
		}
		return f, nil
	}
	var cfg analysis.CapacityConfig
	var err error
	if cfg.Default.P95Ms, err = num("P95_MS", 800); err != nil {
		return cfg, err
	}
	if cfg.Default.P99Ms, err = num("P99_MS", 2000); err != nil {
		return cfg, err
	}
	if cfg.Default.ErrorRate, err = num("ERR_RATE", 0.01); err != nil {
		return cfg, err
	}
	if cfg.ConnectP95, err = num("CONNECT_P95_MS", 1500); err != nil {
		return cfg, err
	}
	min, err := num("MIN_AGENTS", 0)
	if err != nil {
		return cfg, err
	}
	if min < 0 || min != math.Trunc(min) {
		return cfg, fmt.Errorf("MIN_AGENTS must be a whole number >= 0, got %v", min)
	}
	cfg.MinAgents = int(min)
	if cfg.AbortErrRate, err = num("ABORT_ERR_RATE", 0.5); err != nil {
		return cfg, err
	}

	type override struct{ P95, P99, ErrRate *float64 }
	overrides := map[string]override{"flaky": {ErrRate: report.F(0.2)}}
	if v := strings.TrimSpace(env["TOOL_BUDGETS"]); v != "" {
		var user map[string]struct {
			P95     *float64 `json:"p95"`
			P99     *float64 `json:"p99"`
			ErrRate *float64 `json:"errRate"`
		}
		if err := json.Unmarshal([]byte(v), &user); err != nil {
			return cfg, fmt.Errorf("TOOL_BUDGETS must be a JSON object: %v", err)
		}
		for name, u := range user {
			overrides[name] = override{u.P95, u.P99, u.ErrRate}
		}
	}
	cfg.Tools = map[string]report.ToolBudget{}
	for name, o := range overrides {
		b := cfg.Default
		if o.P95 != nil {
			b.P95Ms = *o.P95
		}
		if o.P99 != nil {
			b.P99Ms = *o.P99
		}
		if o.ErrRate != nil {
			b.ErrorRate = *o.ErrRate
		}
		cfg.Tools[name] = b
	}
	return cfg, nil
}

// buildSteps converts the per-step aggregates into report steps (measured
// fields only; analysis.CapacityVerdict judges them). Times are relative to
// origin; the generator CPU of a step is the busiest k6 sampling window whose
// midpoint lies within the step (else any window overlapping it).
func buildSteps(in []k6run.StepStats, origin time.Time, cpu []sampler.CPUWindow) []report.Step {
	out := make([]report.Step, 0, len(in))
	for _, s := range in {
		st := report.Step{VUs: s.VUs, StartS: round3(s.First.Sub(origin).Seconds()), EndS: round3(s.Last.Sub(origin).Seconds()),
			Reqs: s.Reqs, Errors: s.Errors}
		if st.StartS < 0 {
			st.StartS = 0
		}
		if s.Reqs > 0 {
			st.ErrorRate = round6(float64(s.Errors) / float64(s.Reqs))
		}
		if span := s.Last.Sub(s.First).Seconds(); span > 0 {
			st.RPS = round3(float64(s.Reqs) / span)
		}
		if s.CallP95 != nil && s.CallP99 != nil {
			st.P95Ms, st.P99Ms = report.F(round3(*s.CallP95)), report.F(round3(*s.CallP99))
		}
		if s.ConnectP95 != nil {
			st.ConnectP95Ms = report.F(round3(*s.ConnectP95))
		}
		if s.Connects > 0 {
			st.ConnectErrorRate = report.F(round6(float64(s.ConnectErrors) / float64(s.Connects)))
		}
		st.GeneratorCPUMaxPct = stepCPU(cpu, s.First, s.Last)
		if len(s.ByErrorType) > 0 {
			st.ByErrorType = s.ByErrorType
		}
		for _, t := range s.Tools {
			tool := report.StepTool{Name: t.Name, Reqs: t.Reqs, Errors: t.Errors, ErrorRate: round6(t.ErrorRate), ByErrorType: t.ByErrorType}
			if t.Errors < t.Reqs { // at least one successful call
				tool.P95, tool.P99 = report.F(round3(t.P95)), report.F(round3(t.P99))
			}
			st.Tools = append(st.Tools, tool)
		}
		out = append(out, st)
	}
	return out
}

func stepCPU(ws []sampler.CPUWindow, from, to time.Time) *float64 {
	var best *float64
	pick := func(within func(w sampler.CPUWindow) bool) {
		for _, w := range ws {
			if within(w) && (best == nil || w.Pct > *best) {
				best = report.F(round3(w.Pct))
			}
		}
	}
	pick(func(w sampler.CPUWindow) bool {
		mid := w.Start.Add(w.End.Sub(w.Start) / 2)
		return !mid.Before(from) && !mid.After(to)
	})
	if best == nil {
		pick(func(w sampler.CPUWindow) bool { return w.End.After(from) && w.Start.Before(to) })
	}
	return best
}

// stepHints are the knobs printSteps suggests turning: the capacity
// subcommand's flags, or STEPS for `run --scenario step-load`.
type stepHints struct{ raise, lower string }

var (
	runStepHints      = stepHints{raise: "add higher STEPS", lower: "start STEPS lower"}
	capacityStepHints = stepHints{raise: "raise --to", lower: "lower --from"}
)

// stepNotes returns the markers of one step: "<- degradation", "<- breaks
// budget" (the first breach), "<- failure", a breach of a later step without
// an arrow, and a saturated load generator.
func stepNotes(c *report.Capacity, st report.Step) []string {
	var notes []string
	more := func(bs []string) string {
		if len(bs) > 1 {
			return fmt.Sprintf("%s (+%d more)", bs[0], len(bs)-1)
		}
		return bs[0]
	}
	if d := c.Degradation; d != nil && d.VUs == st.VUs {
		notes = append(notes, "<- degradation: "+d.Reason)
	}
	switch {
	case c.BreakingVUs != nil && *c.BreakingVUs == st.VUs && len(st.Breaches) > 0:
		notes = append(notes, "<- breaks budget: "+more(st.Breaches))
	case !st.Passed && len(st.Breaches) > 0 && (c.Failure == nil || c.Failure.VUs != st.VUs):
		notes = append(notes, "over budget: "+more(st.Breaches))
	}
	if f := c.Failure; f != nil && f.VUs == st.VUs {
		notes = append(notes, "<- failure: "+f.Reason)
	}
	if st.GeneratorSaturated && st.GeneratorCPUMaxPct != nil {
		notes = append(notes, fmt.Sprintf("(k6 CPU %.0f%%: load generator saturated)", *st.GeneratorCPUMaxPct))
	}
	return notes
}

// stepErrorClasses formats the two most frequent error classes of a step for
// the Errors cell, e.g. "timeout 14, http 3" ("" without errors). tool_iserror
// (a tool that answered isError, such as the demo `flaky`) is left out unless
// a tool with such errors went over its error budget in the step: a tool
// failing at its usual, budgeted rate would otherwise fill every row.
func stepErrorClasses(st report.Step) string {
	m := st.ByErrorType
	if m[k6run.ErrorTypeToolIsError] > 0 {
		keep := false
		for _, t := range st.Tools {
			if t.ByErrorType[k6run.ErrorTypeToolIsError] > 0 && slices.Contains(t.Breached, "errorRate") {
				keep = true
			}
		}
		if !keep {
			m = maps.Clone(m)
			delete(m, k6run.ErrorTypeToolIsError)
		}
	}
	if len(m) == 0 {
		return ""
	}
	if s := analysis.TopErrorTypes(m, 2); s != "-" {
		return s
	}
	return ""
}

// printSteps prints the per-step table of a step-load run (p95/p99 of all
// tools/call, error rate of all requests, req/s, the slowest tool and the
// markers of stepNotes), the max sustainable concurrency and the estimate.
func printSteps(w io.Writer, c *report.Capacity, h stepHints) {
	if c == nil || len(c.Steps) == 0 {
		return
	}
	msOr := func(p *float64) string {
		if p == nil {
			return "-"
		}
		return analysis.FormatMs(*p)
	}
	rows := [][]string{{"Agents", "p95", "p99", "Errors", "req/s", "Slowest tool p95"}}
	var notes [][]string
	refined := false
	// Errors: the rate, right-aligned within the cell, then its top classes.
	rateW := 0
	for _, st := range c.Steps {
		rateW = max(rateW, len(fmt.Sprintf("%.2f%%", 100*st.ErrorRate)))
	}
	for _, st := range c.Steps {
		ag := strconv.Itoa(st.VUs)
		if st.Refinement {
			ag += "*"
			refined = true
		}
		var worst *report.StepTool
		for i, t := range st.Tools {
			if t.P95 != nil && (worst == nil || *t.P95 > *worst.P95) {
				worst = &st.Tools[i]
			}
		}
		slowest := "-"
		if worst != nil {
			slowest = worst.Name + " " + analysis.FormatMs(*worst.P95)
		}
		errs := fmt.Sprintf("%*s", rateW, fmt.Sprintf("%.2f%%", 100*st.ErrorRate))
		if cls := stepErrorClasses(st); cls != "" {
			errs += " " + cls
		}
		rows = append(rows, []string{ag, msOr(st.P95Ms), msOr(st.P99Ms), errs, fmt.Sprintf("%.1f", st.RPS), slowest})
		notes = append(notes, stepNotes(c, st))
	}
	width := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, cell := range r {
			width[i] = max(width[i], len(cell))
		}
	}
	fmt.Fprintf(w, "\nmcpload capacity steps (p95/p99: all tools/call; errors: all requests):\n")
	for i, r := range rows {
		var b strings.Builder
		for j, cell := range r {
			if j == 3 || j == len(r)-1 { // Errors and the slowest tool left-aligned, the numbers right-aligned
				fmt.Fprintf(&b, "  %-*s", width[j], cell)
			} else {
				fmt.Fprintf(&b, "  %*s", width[j], cell)
			}
		}
		if i > 0 && len(notes[i-1]) > 0 {
			b.WriteString("  " + strings.Join(notes[i-1], "; "))
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
	if refined {
		fmt.Fprintln(w, "  * refinement step (second k6 run between the last passing and the first breaking step)")
	}
	switch {
	case c.MaxSustainableVUs != nil && c.BreakingVUs == nil:
		fmt.Fprintf(w, "max sustainable concurrency: at least %d agents (no step broke)\n", *c.MaxSustainableVUs)
	case c.MaxSustainableVUs != nil && c.Inconclusive:
		fmt.Fprintf(w, "max sustainable concurrency: at least %d agents (inconclusive at %d: load generator saturated)\n", *c.MaxSustainableVUs, *c.BreakingVUs)
	case c.MaxSustainableVUs != nil:
		fmt.Fprintf(w, "max sustainable concurrency: %d agents (budgets broke at %d)\n", *c.MaxSustainableVUs, *c.BreakingVUs)
	default:
		fmt.Fprintf(w, "max sustainable concurrency: none (budgets broke at the first step, %d agents)\n", *c.BreakingVUs)
	}
	fmt.Fprintf(w, "Estimated sustainable capacity: %s\n", estimateLine(c, h))
}

// estimateLine words the capacity estimate for the terminal.
func estimateLine(c *report.Capacity, h stepHints) string {
	top := c.Steps[len(c.Steps)-1].VUs
	switch {
	case c.EstimatedVUs != nil:
		return fmt.Sprintf("~%d agents (%s; an estimate, not a measured step)", *c.EstimatedVUs, c.EstimateBasis)
	case c.BreakingVUs == nil:
		return fmt.Sprintf("at least %d agents (nothing broke; %s)", top, h.raise)
	case c.Inconclusive:
		return fmt.Sprintf("inconclusive (the load generator was saturated at %d agents; run k6 on a separate or bigger machine)", *c.BreakingVUs)
	default:
		return fmt.Sprintf("below %d agents (the first step broke; %s)", *c.BreakingVUs, h.lower)
	}
}
