package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
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
		if s.ConnectP95 != nil {
			st.ConnectP95Ms = report.F(round3(*s.ConnectP95))
		}
		if s.Connects > 0 {
			st.ConnectErrorRate = report.F(round6(float64(s.ConnectErrors) / float64(s.Connects)))
		}
		st.GeneratorCPUMaxPct = stepCPU(cpu, s.First, s.Last)
		for _, t := range s.Tools {
			tool := report.StepTool{Name: t.Name, Reqs: t.Reqs, Errors: t.Errors, ErrorRate: round6(t.ErrorRate)}
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

// printSteps prints the per-step table of a step-load run.
func printSteps(w io.Writer, c *report.Capacity) {
	if c == nil || len(c.Steps) == 0 {
		return
	}
	fmt.Fprintf(w, "\nmcpload steps (agents, result, req/s, error rate, connect p95, slowest tool p95, k6 CPU):\n")
	for _, st := range c.Steps {
		res := "PASS"
		if !st.Passed {
			res = "BREACH"
		}
		worst, worstName := -1.0, ""
		for _, t := range st.Tools {
			if t.P95 != nil && *t.P95 > worst {
				worst, worstName = *t.P95, t.Name
			}
		}
		slowest := "-"
		if worstName != "" {
			slowest = worstName + " " + analysis.FormatMs(worst)
		}
		conn := "-"
		if st.ConnectP95Ms != nil {
			conn = analysis.FormatMs(*st.ConnectP95Ms)
		}
		cpu := "-"
		if st.GeneratorCPUMaxPct != nil {
			cpu = fmt.Sprintf("%.0f%%", *st.GeneratorCPUMaxPct)
			if st.GeneratorSaturated {
				cpu += " (saturated)"
			}
		}
		fmt.Fprintf(w, "  %6d  %-6s  %8.1f  %7.2f%%  %9s  %-22s  %s\n", st.VUs, res, st.RPS, 100*st.ErrorRate, conn, slowest, cpu)
		if !st.Passed {
			fmt.Fprintf(w, "          %s\n", strings.Join(st.Breaches, "; "))
		}
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
}
