package analysis

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

// Capacity (scenario "step-load", report/schema/README.md "Verdict ids").
//
// The step-load scenario raises concurrency in steps and tags every request
// made while a step holds its level. Each step is judged on its own against
// the budgets the other scenarios turn into k6 thresholds:
//
//   - per tool with at least CapacityMinToolCalls calls in the step: p95 and
//     p99 of successful calls and the error rate against P95_MS / P99_MS /
//     ERR_RATE, with TOOL_BUDGETS overrides (same rule as k6: value >= budget
//     is a breach);
//   - session starts: connect p95 against CONNECT_P95_MS, and the share of
//     failed connects against ERR_RATE;
//   - a step without any tools/call is a breach (sessions never got going).
//
// An error-rate breach names the tool's dominant error class (error_type),
// e.g. "`slow` error rate 12% > 1%, mostly `timeout` (14 of 17)".
//
// The breaking point is the first step with a breach; the max sustainable
// concurrency is the step before it (the highest step when none broke). When
// k6 itself was saturated at the breaking step (busiest sampling interval
// above GeneratorCPUWarnPct of the machine), the breach may be generator
// overhead: the result is inconclusive rather than blamed on the server.
//
// Status:
//   - MinAgents set: pass when the max sustainable concurrency >= MinAgents;
//     fail when a conclusive breach happened at or below MinAgents (or at
//     the first step); warn
//     otherwise (inconclusive at or below the target, or the target was never
//     reached or lies between the last passing and the breaking step).
//   - MinAgents unset (informational): pass when no step broke, fail only when
//     the first step broke conclusively (nothing is sustainable: usually a
//     server that is down or a misconfigured run), warn otherwise.
//   - skipped when no request carried a step tag.
//
// Beyond the verdict, the capacity block carries an estimate between the last
// passing and the breaking step (see estimate) and marks the first step that
// degraded while still holding its budgets (see degradation) and the first
// step where the server fell over (see failure).
const CapacityMinToolCalls = 10

// CapacityConfig holds the budgets and run facts for CapacityVerdict.
type CapacityConfig struct {
	Default    report.ToolBudget
	Tools      map[string]report.ToolBudget // complete per-tool overrides
	ConnectP95 float64                      // ms
	MinAgents  int                          // 0 = unset
	Planned    []int                        // planned step levels (k6 inspect)
	// StoppedEarly: the scenario aborted the run (ABORT_ERR_RATE guard).
	StoppedEarly bool
	// AbortErrRate is the scenario's ABORT_ERR_RATE; a step whose error rate
	// reaches it is marked as the failure point (0 = the default, 0.5).
	AbortErrRate float64
}

// Budget returns the budget of one tool.
func (c CapacityConfig) Budget(tool string) report.ToolBudget {
	if b, ok := c.Tools[tool]; ok {
		return b
	}
	return c.Default
}

type breach struct {
	ratio float64 // observed / budget, for ordering
	text  string
	// metric reads the breached measurement from any step (false when that
	// step lacks it; nil when it cannot be interpolated); budget is its limit
	// and what names it, e.g. "`slow` p95" (for the capacity estimate).
	metric func(report.Step) (float64, bool)
	budget float64
	what   string
	limit  string // budget formatted, e.g. "800 ms"
}

// stepTool returns a reader of one field of the named tool in a step.
func stepTool(name string, field func(report.StepTool) *float64) func(report.Step) (float64, bool) {
	return func(st report.Step) (float64, bool) {
		for _, t := range st.Tools {
			if t.Name == name {
				if p := field(t); p != nil {
					return *p, true
				}
			}
		}
		return 0, false
	}
}

func stepField(field func(report.Step) *float64) func(report.Step) (float64, bool) {
	return func(st report.Step) (float64, bool) {
		if p := field(st); p != nil {
			return *p, true
		}
		return 0, false
	}
}

// FormatMs formats a latency like the HTML report: "850 ms", "1.9 s".
func FormatMs(v float64) string {
	if v >= 1000 {
		return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.2f", v/1000), "0"), ".0") + " s"
	}
	if v < 10 {
		return fmt.Sprintf("%.1f ms", v)
	}
	return fmt.Sprintf("%.0f ms", v)
}

func fmtPct(v float64) string {
	return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.2f", 100*v), "0"), ".0") + "%"
}

func ratioOf(v, budget float64) float64 {
	if budget <= 0 {
		return math.Inf(1)
	}
	return v / budget
}

// errorClass is one error_type and its count.
type errorClass struct {
	name string
	n    int64
}

// sortedClasses returns the error types of m, most frequent first (ties by
// name), without zero counts.
func sortedClasses(m map[string]int64) []errorClass {
	var cs []errorClass
	for k, v := range m {
		if v > 0 {
			cs = append(cs, errorClass{k, v})
		}
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].n != cs[j].n {
			return cs[i].n > cs[j].n
		}
		return cs[i].name < cs[j].name
	})
	return cs
}

// TopErrorTypes formats the n most frequent error types of m, e.g.
// "timeout 14, http 3" (with "+k more" when there are more); "-" when empty.
func TopErrorTypes(m map[string]int64, n int) string {
	cs := sortedClasses(m)
	if len(cs) == 0 {
		return "-"
	}
	var parts []string
	for i, c := range cs {
		if i == n {
			parts = append(parts, fmt.Sprintf("+%d more", len(cs)-n))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %d", c.name, c.n))
	}
	return strings.Join(parts, ", ")
}

// dominantClass names the error class behind an error-rate breach:
// ", all `timeout`", ", mostly `timeout` (14 of 17)", or "" when unknown.
func dominantClass(m map[string]int64) string {
	cs := sortedClasses(m)
	if len(cs) == 0 {
		return ""
	}
	if len(cs) == 1 {
		return fmt.Sprintf(", all `%s`", cs[0].name)
	}
	var total int64
	for _, c := range cs {
		total += c.n
	}
	word := "mostly"
	if 2*cs[0].n <= total {
		word = "mainly"
	}
	return fmt.Sprintf(", %s `%s` (%d of %d)", word, cs[0].name, cs[0].n, total)
}

// judgeStep sets Passed, Breached, Breaches and GeneratorSaturated of st and
// returns its breaches, worst first.
func judgeStep(st *report.Step, cfg CapacityConfig) []breach {
	var bs []breach
	st.Breached = nil
	if p := st.ConnectP95Ms; p != nil && *p >= cfg.ConnectP95 {
		st.Breached = append(st.Breached, "connectP95Ms")
		bs = append(bs, breach{ratioOf(*p, cfg.ConnectP95), fmt.Sprintf("connect p95 %s > %s", FormatMs(*p), FormatMs(cfg.ConnectP95)),
			stepField(func(s report.Step) *float64 { return s.ConnectP95Ms }), cfg.ConnectP95, "connect p95", FormatMs(cfg.ConnectP95)})
	}
	if r := st.ConnectErrorRate; r != nil && *r >= cfg.Default.ErrorRate {
		st.Breached = append(st.Breached, "connectErrorRate")
		bs = append(bs, breach{ratioOf(*r, cfg.Default.ErrorRate), fmt.Sprintf("%s of session starts failed (budget %s)", fmtPct(*r), fmtPct(cfg.Default.ErrorRate)),
			stepField(func(s report.Step) *float64 { return s.ConnectErrorRate }), cfg.Default.ErrorRate, "failed session starts", fmtPct(cfg.Default.ErrorRate)})
	}
	var calls int64
	for i := range st.Tools {
		t := &st.Tools[i]
		t.Breached = nil
		calls += t.Reqs
		if t.Reqs < CapacityMinToolCalls {
			continue
		}
		b := cfg.Budget(t.Name)
		if t.P95 != nil && *t.P95 >= b.P95Ms {
			t.Breached = append(t.Breached, "p95")
			bs = append(bs, breach{ratioOf(*t.P95, b.P95Ms), fmt.Sprintf("`%s` p95 %s > %s", t.Name, FormatMs(*t.P95), FormatMs(b.P95Ms)),
				stepTool(t.Name, func(t report.StepTool) *float64 { return t.P95 }), b.P95Ms, "`" + t.Name + "` p95", FormatMs(b.P95Ms)})
		}
		if t.P99 != nil && *t.P99 >= b.P99Ms {
			t.Breached = append(t.Breached, "p99")
			bs = append(bs, breach{ratioOf(*t.P99, b.P99Ms), fmt.Sprintf("`%s` p99 %s > %s", t.Name, FormatMs(*t.P99), FormatMs(b.P99Ms)),
				stepTool(t.Name, func(t report.StepTool) *float64 { return t.P99 }), b.P99Ms, "`" + t.Name + "` p99", FormatMs(b.P99Ms)})
		}
		if t.ErrorRate >= b.ErrorRate {
			t.Breached = append(t.Breached, "errorRate")
			bs = append(bs, breach{ratioOf(t.ErrorRate, b.ErrorRate), fmt.Sprintf("`%s` error rate %s > %s%s", t.Name, fmtPct(t.ErrorRate), fmtPct(b.ErrorRate), dominantClass(t.ByErrorType)),
				stepTool(t.Name, func(t report.StepTool) *float64 { return &t.ErrorRate }), b.ErrorRate, "`" + t.Name + "` error rate", fmtPct(b.ErrorRate)})
		}
	}
	if calls == 0 {
		st.Breached = append(st.Breached, "toolCalls")
		bs = append(bs, breach{ratio: math.Inf(1), text: "no tools/call completed (sessions never got going)"})
	}
	sort.SliceStable(bs, func(i, j int) bool { return bs[i].ratio > bs[j].ratio })
	st.Breaches = make([]string, len(bs))
	for i, b := range bs {
		st.Breaches[i] = b.text
	}
	st.Passed = len(bs) == 0
	st.GeneratorSaturated = st.GeneratorCPUMaxPct != nil && *st.GeneratorCPUMaxPct > GeneratorCPUWarnPct
	return bs
}

func agents(n int) string {
	if n == 1 {
		return "1 agent"
	}
	return fmt.Sprintf("%d agents", n)
}

// CapacityVerdict judges each step of a step-load run (in place: Passed,
// Breached, Breaches, GeneratorSaturated) and returns the capacity summary and
// verdict. steps must be sorted by VUs.
func CapacityVerdict(steps []report.Step, cfg CapacityConfig) (*report.Capacity, report.Verdict) {
	const id, signal = report.VerdictCapacity, "capacity.steps"
	c := &report.Capacity{PlannedVUs: cfg.Planned, StoppedEarly: cfg.StoppedEarly, Steps: steps,
		Budgets: report.CapacityBudgets{ConnectP95Ms: cfg.ConnectP95, ConnectErrorRate: cfg.Default.ErrorRate, Tools: map[string]report.ToolBudget{}}}
	if cfg.MinAgents > 0 {
		c.MinAgents = report.I(cfg.MinAgents)
	}
	if len(steps) == 0 {
		return c, skipped(id, signal, "no request carried a step tag (run the step-load scenario)")
	}
	brk := -1
	var bs []breach
	for i := range steps {
		for _, t := range steps[i].Tools {
			c.Budgets.Tools[t.Name] = cfg.Budget(t.Name)
		}
		b := judgeStep(&steps[i], cfg)
		if brk < 0 && len(b) > 0 {
			brk, bs = i, b
		}
	}
	top := steps[len(steps)-1].VUs
	c.Degradation = degradation(steps, brk, cfg)
	c.Failure = failure(steps, cfg)
	switch {
	case brk < 0:
		c.EstimateBasis = fmt.Sprintf("nothing broke up to %s (the highest step tested)", agents(top))
	case steps[brk].GeneratorSaturated:
		c.EstimateBasis = fmt.Sprintf("inconclusive: the load generator was saturated at %s", agents(steps[brk].VUs))
	case brk == 0:
		c.EstimateBasis = fmt.Sprintf("the first step (%s) already broke budgets", agents(steps[0].VUs))
	default:
		est, basis := estimate(steps[brk-1], steps[brk], bs)
		c.EstimatedVUs, c.EstimateBasis = report.I(est), basis
	}
	v := report.Verdict{ID: id, Signal: signal, Status: report.StatusPass}
	var msg string
	if brk < 0 {
		c.MaxSustainableVUs = report.I(top)
		msg = fmt.Sprintf("Held budgets at every step up to %s (the highest step tested); the breaking point is higher.", agents(top))
	} else {
		st := steps[brk]
		c.BreakingVUs = report.I(st.VUs)
		c.Inconclusive = st.GeneratorSaturated
		if brk > 0 {
			c.MaxSustainableVUs = report.I(steps[brk-1].VUs)
		}
		what := bs[0].text
		if len(bs) > 1 {
			what += fmt.Sprintf(" (+%d more)", len(bs)-1)
		}
		held := "No step held its budgets"
		if brk > 0 {
			held = "Held budgets up to " + agents(steps[brk-1].VUs)
		}
		if c.Inconclusive {
			msg = fmt.Sprintf("Inconclusive at %s: the load generator was saturated (k6 up to %.0f%% CPU), so %s may be k6's own overhead. %s. Run k6 on a separate or bigger machine to find the server's limit.",
				agents(st.VUs), *st.GeneratorCPUMaxPct, what, held)
		} else if brk == 0 {
			msg = fmt.Sprintf("Broke budgets at the first step (%s): %s. No sustainable concurrency found.", agents(st.VUs), what)
		} else {
			msg = fmt.Sprintf("%s; at %s %s.", held, agents(st.VUs), what)
		}
	}
	if cfg.StoppedEarly {
		var notRun []string
		for _, p := range cfg.Planned {
			if p > top {
				notRun = append(notRun, fmt.Sprint(p))
			}
		}
		msg += fmt.Sprintf(" The run stopped early at %s (ABORT_ERR_RATE)", agents(top))
		if len(notRun) > 0 {
			msg += "; steps not run: " + strings.Join(notRun, ", ")
		}
		msg += "."
	}

	maxOK := 0
	if c.MaxSustainableVUs != nil {
		maxOK = *c.MaxSustainableVUs
	}
	switch {
	case cfg.MinAgents > 0 && maxOK >= cfg.MinAgents:
		msg += fmt.Sprintf(" Target MIN_AGENTS=%d met.", cfg.MinAgents)
	case cfg.MinAgents > 0 && brk >= 0 && (steps[brk].VUs <= cfg.MinAgents || brk == 0) && !c.Inconclusive:
		v.Status = report.StatusFail
		msg += fmt.Sprintf(" Target MIN_AGENTS=%d not met.", cfg.MinAgents)
	case cfg.MinAgents > 0 && brk >= 0 && steps[brk].VUs <= cfg.MinAgents:
		v.Status = report.StatusWarn
		msg += fmt.Sprintf(" Target MIN_AGENTS=%d not confirmed.", cfg.MinAgents)
	case cfg.MinAgents > 0 && brk >= 0:
		v.Status = report.StatusWarn
		msg += fmt.Sprintf(" Target MIN_AGENTS=%d lies between the last passing step and the breaking step; add a step at %d to check it.", cfg.MinAgents, cfg.MinAgents)
	case cfg.MinAgents > 0:
		v.Status = report.StatusWarn
		msg += fmt.Sprintf(" Target MIN_AGENTS=%d was not reached: the highest step tested was %d.", cfg.MinAgents, top)
	case brk == 0 && !c.Inconclusive:
		v.Status = report.StatusFail
	case brk >= 0:
		v.Status = report.StatusWarn
	}
	v.Message = msg
	return c, v
}

// estimate interpolates the sustainable concurrency between the last passing
// step (held) and the first breaking step (broke): for every breached metric
// measured in both steps, the point where the straight line between its two
// values reaches its budget (the held step itself when it was already at the
// budget there, e.g. a tool below CapacityMinToolCalls); the lowest such
// point wins. Latency usually grows faster than linearly near saturation, so
// the line crosses the budget early and the estimate errs low. The result is
// rounded to 2 significant figures and kept within [held, broke).
func estimate(held, broke report.Step, bs []breach) (int, string) {
	n0, n1 := float64(held.VUs), float64(broke.VUs)
	best, what, used := math.Inf(1), "", 0
	for _, b := range bs {
		if b.metric == nil {
			continue
		}
		v0, ok0 := b.metric(held)
		v1, ok1 := b.metric(broke)
		if !ok0 || !ok1 {
			continue
		}
		used++
		x, desc := n0, fmt.Sprintf("%s, already at its %s budget at %d (too few calls there to count as a breach)", b.what, b.limit, held.VUs)
		if v0 < b.budget && v1 > v0 {
			x, desc = n0+(b.budget-v0)/(v1-v0)*(n1-n0), fmt.Sprintf("%s to its %s budget", b.what, b.limit)
		}
		if x < best {
			best, what = x, desc
		}
	}
	bracket := fmt.Sprintf("between %d (held) and %d (broke)", held.VUs, broke.VUs)
	if used == 0 {
		return held.VUs, bracket + "; no breached metric was measured at both steps, so this is the last step that held"
	}
	est := min(max(RoundSig2(best), held.VUs), max(broke.VUs-1, held.VUs))
	basis := bracket + "; linear interpolation of " + what
	if used > 1 {
		basis += fmt.Sprintf(" (the lowest of %d breached metrics)", used)
	}
	return est, basis
}

// RoundSig2 rounds a positive count to 2 significant figures (7.4 -> 7,
// 14.6 -> 15, 254 -> 250, 1349 -> 1300).
func RoundSig2(x float64) int {
	if x < 10 {
		return int(math.Round(x))
	}
	p := math.Pow(10, math.Floor(math.Log10(x))-1)
	return int(math.Round(x/p) * p)
}

// stepP95 is the p95 of a step's successful tools/call (all tools), or the
// slowest tool's p95 for reports written before p95Ms existed.
func stepP95(st report.Step) (float64, bool) {
	if st.P95Ms != nil {
		return *st.P95Ms, true
	}
	worst, ok := 0.0, false
	for _, t := range st.Tools {
		if t.P95 != nil && *t.P95 >= worst {
			worst, ok = *t.P95, true
		}
	}
	return worst, ok
}

// degradation marks the first step after the first one that still held its
// budgets but clearly got worse: the p95 of tools/call (stepP95) reached
// twice the first step's, or a tool (with at least CapacityMinToolCalls calls)
// reached half its error budget and at least twice its error rate at the
// first step, or failed session starts reached half of ERR_RATE (and twice
// the first step's share). nil when no step before the breaking one did.
func degradation(steps []report.Step, brk int, cfg CapacityConfig) *report.StepMark {
	end := len(steps)
	if brk >= 0 {
		end = brk
	}
	base := steps[0]
	baseP95, baseOK := stepP95(base)
	baseErr := map[string]float64{}
	for _, t := range base.Tools {
		baseErr[t.Name] = t.ErrorRate
	}
	worse := func(r, budget, first float64) bool { return budget > 0 && r >= budget/2 && r >= 2*first && r > 0 }
	for i := 1; i < end; i++ {
		st := steps[i]
		if p, ok := stepP95(st); ok && baseOK && baseP95 > 0 && p >= 2*baseP95 {
			return &report.StepMark{VUs: st.VUs, Reason: fmt.Sprintf("p95 %s, %.1fx the first step's %s", FormatMs(p), p/baseP95, FormatMs(baseP95))}
		}
		for _, t := range st.Tools {
			if b := cfg.Budget(t.Name); t.Reqs >= CapacityMinToolCalls && worse(t.ErrorRate, b.ErrorRate, baseErr[t.Name]) {
				return &report.StepMark{VUs: st.VUs, Reason: fmt.Sprintf("`%s` error rate %s, over half its %s budget%s", t.Name, fmtPct(t.ErrorRate), fmtPct(b.ErrorRate), dominantClass(t.ByErrorType))}
			}
		}
		if r := st.ConnectErrorRate; r != nil {
			first := 0.0
			if base.ConnectErrorRate != nil {
				first = *base.ConnectErrorRate
			}
			if worse(*r, cfg.Default.ErrorRate, first) {
				return &report.StepMark{VUs: st.VUs, Reason: fmt.Sprintf("%s of session starts failed, over half the %s budget", fmtPct(*r), fmtPct(cfg.Default.ErrorRate))}
			}
		}
	}
	return nil
}

// failure marks the first step where the server fell over: its error rate
// reached ABORT_ERR_RATE (0.5 when unset or 0), or no tools/call completed;
// failing that, the last step when the scenario stopped the run early.
func failure(steps []report.Step, cfg CapacityConfig) *report.StepMark {
	abort := cfg.AbortErrRate
	if abort <= 0 {
		abort = 0.5
	}
	for _, st := range steps {
		var why string
		switch {
		case st.Reqs > 0 && st.ErrorRate >= abort:
			why = fmt.Sprintf("error rate %s >= %s%s", fmtPct(st.ErrorRate), fmtPct(abort), dominantClass(st.ByErrorType))
		case slices.Contains(st.Breached, "toolCalls"):
			why = "no tools/call completed"
		default:
			continue
		}
		if cfg.StoppedEarly && st.VUs == steps[len(steps)-1].VUs {
			why += "; the run stopped here (ABORT_ERR_RATE)"
		}
		return &report.StepMark{VUs: st.VUs, Reason: why}
	}
	if cfg.StoppedEarly {
		return &report.StepMark{VUs: steps[len(steps)-1].VUs, Reason: fmt.Sprintf("the run stopped here: a VU saw more than %s of its calls fail (ABORT_ERR_RATE)", fmtPct(abort))}
	}
	return nil
}

// SkipDriftForSteps replaces latency_drift and error_drift with skipped
// verdicts: a step-load run raises the load on purpose, so a rising latency
// or error rate is the capacity verdict's subject, not drift.
func SkipDriftForSteps(vs []report.Verdict) []report.Verdict {
	for i, v := range vs {
		if v.ID == report.VerdictLatencyDrift || v.ID == report.VerdictErrorDrift {
			vs[i] = skipped(v.ID, v.Signal, "a step-load run raises the load step by step; see the capacity verdict")
		}
	}
	return vs
}
