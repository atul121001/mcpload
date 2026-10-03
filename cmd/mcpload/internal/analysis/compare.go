package analysis

// Baseline comparison (mcpload compare, mcpload run --baseline): this run's
// per-tool and run-level numbers against a baseline report, with rules that
// keep CI-runner noise from reading as a regression.
//
//   - Latency (per tool p95/p99, connect p95): regressed when the rise is more
//     than the relative limit (MaxP95Increase / MaxP99Increase of the baseline)
//     AND more than MinDeltaMs. A 4 ms tool going to 6 ms is +50% but noise;
//     a 2 s tool gaining 30 ms is over the floor but only +1.5%. Improved when
//     the fall passes the same two limits.
//   - Error rate (per tool and overall): regressed when the rate rises by more
//     than MinErrorDelta (absolute, 0.5 points by default) AND by more than
//     MaxErrorIncrease of the baseline rate AND a one-sided two-proportion
//     z-test says the rise is unlikely to be chance (z >= 1.645, p < 0.05).
//     The test is what keeps 0/60 -> 1/60 (+1.7 points) from failing a build.
//   - Per tool, nothing is judged unless both runs made at least MinCalls
//     calls of it (latency: MinCalls successful calls); the tool is listed as
//     few_calls. Tools in only one report are added/removed, never regressions.
//   - Memory (both runs sampled with the same sampler): RSS growth over the
//     leak window (from LeakSettleS or the end of warm-up, OLS on the lower
//     envelope, as memory_leak) regressed when it grew by more than
//     MemMinGrowthMiB (5 MiB) more than the baseline's with R² >= MinR2, or
//     when memory_leak went from pass/warn to fail. Runs with less than
//     LeakMinLoadS (2 min) in that window show their growth but are not
//     judged: in a 30 s run, heap warm-up alone is several MiB. Leak slope
//     regressed when it rose by more than MaxLeakSlopeIncrease MiB/min and by
//     at least the growth floor over the window. Retained memory after
//     cool-down regressed when it is more than the growth floor above the
//     baseline's.
//   - Capacity (both step-load runs): regressed when the max sustainable
//     concurrency dropped and neither run was inconclusive.
//   - Scenario, protocol, load shape, load duration (more than 1.5x apart), sampler and
//     a saturated load generator are compared too; a difference is a warning
//     that the comparison may be unfair, not a refusal.

import (
	"fmt"
	"math"
	"strings"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

// CompareConfig holds the comparison rules (report.CompareRules plus fixed limits).
type CompareConfig struct {
	MaxP95Increase       float64 // relative, 0.2 = +20%
	MaxP99Increase       float64 // relative
	MaxErrorIncrease     float64 // relative to the baseline error rate
	MinErrorDelta        float64 // absolute, 0.005 = 0.5 percentage points
	MinDeltaMs           float64 // absolute latency floor
	MinCalls             int64   // per tool, in both runs
	MaxLeakSlopeIncrease float64 // MiB/min
}

// DefaultCompareConfig returns the default comparison rules.
func DefaultCompareConfig() CompareConfig {
	return CompareConfig{MaxP95Increase: 0.20, MaxP99Increase: 0.30, MaxErrorIncrease: 0.50, MinErrorDelta: 0.005,
		MinDeltaMs: 25, MinCalls: 50, MaxLeakSlopeIncrease: 1}
}

// Rules returns cc as the report's comparison.rules.
func (cc CompareConfig) Rules() report.CompareRules {
	return report.CompareRules{MaxP95Increase: cc.MaxP95Increase, MaxP99Increase: cc.MaxP99Increase, MaxErrorIncrease: cc.MaxErrorIncrease,
		MinErrorDelta: cc.MinErrorDelta, MinDeltaMs: cc.MinDeltaMs, MinCalls: cc.MinCalls, MaxLeakSlopeIncrease: cc.MaxLeakSlopeIncrease}
}

const (
	// errorZ is the one-sided 95% critical value of the two-proportion z-test.
	errorZ = 1.645
	// durationMismatch: load windows whose ratio exceeds this get a warning. Without
	// phases the window is the whole run, including k6's graceful stop (up to 30 s
	// more when calls are slow), so the margin is wide.
	durationMismatch = 1.5
)

// Metric ids of comparison.metrics.
const (
	MetricErrorRate      = "errorRate"
	MetricConnectP95     = "connectP95Ms"
	MetricMemoryGrowth   = "memoryGrowthMiB"
	MetricLeakSlope      = "leakSlopeMiBPerMin"
	MetricRetained       = "retainedMiB"
	MetricMaxSustainable = "maxSustainableVus"
)

// latencyDelta judges a latency (ms): regressed when it rose by more than
// maxInc of the baseline and by more than minMs; improved when it fell by both.
func latencyDelta(base, cur, maxInc, minMs float64) string {
	d := cur - base
	switch {
	case d > minMs && d > maxInc*base:
		return report.DeltaRegressed
	case -d > minMs && -d > maxInc*base:
		return report.DeltaImproved
	}
	return report.DeltaOK
}

// twoProportionZ is the z statistic of the current error rate minus the
// baseline's under the pooled null hypothesis; 0 when undefined.
func twoProportionZ(be, bn, ce, cn int64) float64 {
	if bn <= 0 || cn <= 0 {
		return 0
	}
	pb, pc := float64(be)/float64(bn), float64(ce)/float64(cn)
	p := float64(be+ce) / float64(bn+cn)
	se := math.Sqrt(p * (1 - p) * (1/float64(bn) + 1/float64(cn)))
	if se == 0 {
		return 0
	}
	return (pc - pb) / se
}

// errorDelta judges an error rate from error and call counts (see the package rules).
func errorDelta(be, bn, ce, cn int64, cc CompareConfig) string {
	pb, pc := ratioOf64(be, bn), ratioOf64(ce, cn)
	d := pc - pb
	z := twoProportionZ(be, bn, ce, cn)
	switch {
	case d > cc.MinErrorDelta && d > cc.MaxErrorIncrease*pb && z >= errorZ:
		return report.DeltaRegressed
	case -d > cc.MinErrorDelta && -d > cc.MaxErrorIncrease*pb && z <= -errorZ:
		return report.DeltaImproved
	}
	return report.DeltaOK
}

func ratioOf64(a, b int64) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// loadSeconds is the time with load (warm-up + load; cool-down has none).
func loadSeconds(r *report.Report) float64 {
	if s := r.Phases.LoadEndS; s > 0 {
		return s
	}
	return r.Run.DurationS
}

func toolSample(t report.ToolStats, r *report.Report) *report.ToolSample {
	s := &report.ToolSample{Reqs: t.Reqs, Errors: t.Errors, ErrorRate: t.ErrorRate, P50: t.P50, P95: t.P95, P99: t.P99}
	if d := loadSeconds(r); d > 0 {
		s.RPS = round(float64(t.Reqs)/d, 3)
	}
	return s
}

// PctChange formats (cur-base)/base as "+35%": "0%" when it rounds to zero,
// "new" when base is 0 and "" when base is negative (retained memory can be).
func PctChange(base, cur float64) string {
	switch {
	case base < 0:
		return ""
	case base == 0 && cur == 0:
		return "0%"
	case base == 0:
		return "new"
	}
	p := math.Round(100 * (cur - base) / base)
	if p == 0 {
		return "0%"
	}
	return fmt.Sprintf("%+.0f%%", p)
}

// FormatDelta formats a value change in unit (rate, ms, MiB, MiB/min, agents) as "+74 ms" / "+0.70 pts".
func FormatDelta(unit string, d float64) string {
	sign := "+"
	if d < 0 {
		sign, d = "-", -d
	}
	// Changes that round to zero print without a sign.
	if (unit == "rate" && d < 0.00005) || (unit == "ms" && d < 0.05) || (unit == "MiB" && d < 0.05) || (unit == "MiB/min" && d < 0.005) || d == 0 {
		switch unit {
		case "rate":
			return "0 pts"
		case "ms":
			return "0 ms"
		}
		return "0 " + unit
	}
	switch unit {
	case "rate":
		return fmt.Sprintf("%s%.2f pts", sign, 100*d)
	case "ms":
		return sign + FormatMs(d)
	case "MiB":
		return fmt.Sprintf("%s%.1f MiB", sign, d)
	case "MiB/min":
		return fmt.Sprintf("%s%.2f MiB/min", sign, d)
	}
	return fmt.Sprintf("%s%g %s", sign, d, unit)
}

// FormatValue formats a value in unit (see FormatDelta); "–" for nil.
func FormatValue(unit string, v *float64) string {
	if v == nil {
		return "–"
	}
	switch unit {
	case "rate":
		return fmtPct(*v)
	case "ms":
		return FormatMs(*v)
	case "MiB":
		return fmt.Sprintf("%.1f MiB", *v)
	case "MiB/min":
		return fmt.Sprintf("%.2f MiB/min", *v)
	}
	return fmt.Sprintf("%g %s", *v, unit)
}

// change describes base -> cur for a reason: "210 ms → 284 ms (+35%, +74 ms)".
func change(unit string, base, cur float64) string {
	d := FormatDelta(unit, cur-base)
	if p := PctChange(base, cur); p != "" {
		d = p + ", " + d
	}
	return fmt.Sprintf("%s → %s (%s)", FormatValue(unit, &base), FormatValue(unit, &cur), d)
}

// memStats are one run's memory numbers for the comparison.
type memStats struct {
	growth, r2 float64 // fitted RSS growth (MiB) over the leak window (load window when too short) and its R²
	fitted     bool
	judged     bool     // the run is long enough to judge memory (growth is over the leak window)
	slope      *float64 // leak-window slope (MiB/min), when judged
	minutes    float64  // leak-window minutes
	retained   *float64 // cool-down mean minus the post-warm-up baseline (MiB)
	leak       string   // memory_leak status
}

func memoryStats(r *report.Report, cfg Config) memStats {
	var m memStats
	for _, v := range r.Verdicts {
		if v.ID == report.VerdictMemoryLeak {
			m.leak = v.Status
		}
	}
	ys := r.Series.Server.RSSBytes
	// As memory_leak: never before LeakSettleS, and at least LeakMinLoadS of
	// load, or startup growth reads as a regression. A cool-down is not needed.
	from := math.Max(r.Phases.WarmupEndS, cfg.LeakSettleS)
	if r.Phases.LoadEndS-from < cfg.LeakMinLoadS {
		// Too short to judge: show the growth over the whole load window only.
		if f, ok := fitWindow(r, ys, cfg, r.Phases.WarmupEndS, cfg.EnvelopeBuckets); ok {
			m.fitted, m.growth, m.r2 = true, round(f.slope*f.minutes/mib, 2), f.r2
		}
		return m
	}
	f, ok := fitWindow(r, ys, cfg, from, cfg.EnvelopeBuckets)
	if !ok {
		return m
	}
	m.fitted, m.judged, m.growth, m.r2 = true, true, round(f.slope*f.minutes/mib, 2), f.r2
	m.slope, m.minutes = report.F(round(f.slope/mib, 3)), f.minutes
	if _, cool := points(r.Series.T, ys, r.Phases.LoadEndS+cfg.CooldownSkipS, r.Phases.CooldownEndS, true); len(cool) > 0 {
		var sum float64
		for _, v := range cool {
			sum += v
		}
		m.retained = report.F(round((sum/float64(len(cool))-f.baseline)/mib, 2))
	}
	return m
}

// connectP95 is the observed p95 of the mcp_connect_duration threshold, when the scenario set one.
func connectP95(r *report.Report) *float64 {
	for _, t := range r.Thresholds {
		if t.Metric == "mcp_connect_duration" && strings.Contains(t.Expr, "p(95)") && t.Observed != nil {
			return report.F(*t.Observed)
		}
	}
	return nil
}

func loadText(l report.Load) string {
	s := l.Executor
	if l.VUs != nil {
		s += fmt.Sprintf(" %d VUs", *l.VUs)
	}
	if l.ArrivalRate != nil {
		unit := 1.0
		if l.ArrivalTimeUnitS != nil {
			unit = *l.ArrivalTimeUnitS
		}
		s += fmt.Sprintf(" %g/%gs", *l.ArrivalRate, unit)
	}
	if l.MaxVUs != nil {
		s += fmt.Sprintf(" (max %d VUs)", *l.MaxVUs)
	}
	return s
}

func verdictStatus(r *report.Report, id string) string {
	for _, v := range r.Verdicts {
		if v.ID == id {
			return v.Status
		}
	}
	return ""
}

// mismatches lists differences between the runs that may make the comparison unfair.
func mismatches(base, cur *report.Report) []string {
	var w []string
	unfair := "; the comparison may be unfair"
	if base.Run.Scenario != cur.Run.Scenario {
		w = append(w, fmt.Sprintf("scenario differs (baseline %s, current %s)%s", base.Run.Scenario, cur.Run.Scenario, unfair))
	}
	if base.Run.Protocol != cur.Run.Protocol {
		w = append(w, fmt.Sprintf("protocol differs (baseline %s, current %s)%s", base.Run.Protocol, cur.Run.Protocol, unfair))
	}
	if bl, cl := loadText(base.Run.Load), loadText(cur.Run.Load); bl != cl {
		w = append(w, fmt.Sprintf("load differs (baseline %s, current %s)%s", bl, cl, unfair))
	}
	bd, cd := base.Phases.LoadEndS-base.Phases.WarmupEndS, cur.Phases.LoadEndS-cur.Phases.WarmupEndS
	if bd > 0 && cd > 0 && math.Max(bd, cd) > durationMismatch*math.Min(bd, cd) {
		w = append(w, fmt.Sprintf("load duration differs (baseline %.0f s, current %.0f s)%s", bd, cd, unfair))
	}
	bs, cs := base.Series.Server.Sampler, cur.Series.Server.Sampler
	if bs != cs && bs != report.SamplerNone && cs != report.SamplerNone {
		w = append(w, fmt.Sprintf("sampler differs (baseline %s, current %s): memory is not compared, the two measure RSS differently", bs, cs))
	}
	for _, x := range []struct {
		name string
		r    *report.Report
	}{{"baseline", base}, {"current", cur}} {
		if st := verdictStatus(x.r, report.VerdictGenerator); st == report.StatusWarn || st == report.StatusFail {
			w = append(w, fmt.Sprintf("the load generator was saturated in the %s run (generator verdict %s); its latencies may include k6 overhead", x.name, st))
		}
	}
	return w
}

// Compare compares cur with base (read from source) under cc. Memory rules use cfg's leak limits.
func Compare(base, cur *report.Report, source string, cc CompareConfig, cfg Config) *report.Comparison {
	c := &report.Comparison{
		Baseline: report.BaselineRef{Source: source, RunID: base.Run.ID, StartedAt: base.Run.StartedAt, Scenario: base.Run.Scenario,
			Protocol: base.Run.Protocol, Git: base.Run.Git},
		Rules:    cc.Rules(),
		Reasons:  []string{},
		Warnings: mismatches(base, cur),
		Tools:    []report.ToolDelta{},
		Metrics:  []report.MetricDelta{},
	}
	if c.Warnings == nil {
		c.Warnings = []string{}
	}
	reason := func(format string, a ...any) { c.Reasons = append(c.Reasons, fmt.Sprintf(format, a...)) }

	// Tools: the current run's order, then tools only the baseline had.
	baseByName := map[string]report.ToolStats{}
	for _, t := range base.Tools {
		baseByName[t.Name] = t
	}
	curNames := map[string]bool{}
	for _, t := range cur.Tools {
		curNames[t.Name] = true
		td := report.ToolDelta{Name: t.Name, Current: toolSample(t, cur)}
		b, ok := baseByName[t.Name]
		if !ok {
			td.Status = report.DeltaAdded
			c.Tools = append(c.Tools, td)
			continue
		}
		td.Base = toolSample(b, base)
		if b.Reqs < cc.MinCalls || t.Reqs < cc.MinCalls {
			td.Status = report.DeltaFewCalls
			c.Tools = append(c.Tools, td)
			continue
		}
		judge := func(field, st, what string) {
			switch st {
			case report.DeltaRegressed:
				td.Regressed = append(td.Regressed, field)
				reason("`%s` %s", t.Name, what)
			case report.DeltaImproved:
				td.Improved = append(td.Improved, field)
			}
		}
		if b.Reqs-b.Errors >= cc.MinCalls && t.Reqs-t.Errors >= cc.MinCalls {
			judge("p95", latencyDelta(b.P95, t.P95, cc.MaxP95Increase, cc.MinDeltaMs), "p95 "+change("ms", b.P95, t.P95))
			judge("p99", latencyDelta(b.P99, t.P99, cc.MaxP99Increase, cc.MinDeltaMs), "p99 "+change("ms", b.P99, t.P99))
		}
		judge("errorRate", errorDelta(b.Errors, b.Reqs, t.Errors, t.Reqs, cc), "error rate "+change("rate", b.ErrorRate, t.ErrorRate))
		switch {
		case len(td.Regressed) > 0:
			td.Status = report.DeltaRegressed
		case len(td.Improved) > 0:
			td.Status = report.DeltaImproved
		default:
			td.Status = report.DeltaOK
		}
		c.Tools = append(c.Tools, td)
	}
	for _, b := range base.Tools {
		if !curNames[b.Name] {
			c.Tools = append(c.Tools, report.ToolDelta{Name: b.Name, Status: report.DeltaRemoved, Base: toolSample(b, base)})
		}
	}

	metric := func(id, label, unit string, bv, cv *float64, st, note string) {
		if bv == nil || cv == nil {
			st = report.DeltaNA
		}
		c.Metrics = append(c.Metrics, report.MetricDelta{ID: id, Label: label, Unit: unit, Base: bv, Current: cv, Status: st, Note: note})
		if st == report.DeltaRegressed {
			if note != "" {
				reason("%s %s: %s", label, change(unit, *bv, *cv), note)
			} else {
				reason("%s %s", label, change(unit, *bv, *cv))
			}
		}
	}

	bs, cs := base.Summary, cur.Summary
	st := report.DeltaNA
	if bs.Reqs >= cc.MinCalls && cs.Reqs >= cc.MinCalls {
		st = errorDelta(bs.Errors, bs.Reqs, cs.Errors, cs.Reqs, cc)
	}
	metric(MetricErrorRate, "error rate", "rate", report.F(bs.ErrorRate), report.F(cs.ErrorRate), st, "")
	if st == report.DeltaNA {
		c.Metrics[len(c.Metrics)-1].Note = fmt.Sprintf("fewer than %d requests", cc.MinCalls)
	}

	if bp, cp := connectP95(base), connectP95(cur); bp != nil || cp != nil {
		st := report.DeltaNA
		if bp != nil && cp != nil {
			st = latencyDelta(*bp, *cp, cc.MaxP95Increase, cc.MinDeltaMs)
		}
		metric(MetricConnectP95, "connect p95", "ms", bp, cp, st, "")
	}

	samplers := base.Series.Server.Sampler != report.SamplerNone && cur.Series.Server.Sampler != report.SamplerNone
	if samplers && base.Series.Server.Sampler == cur.Series.Server.Sampler {
		bm, cm := memoryStats(base, cfg), memoryStats(cur, cfg)
		floor := cfg.MemMinGrowthMiB
		var bg, cg *float64
		if bm.fitted {
			bg = report.F(bm.growth)
		}
		if cm.fitted {
			cg = report.F(cm.growth)
		}
		st, note := report.DeltaOK, ""
		switch {
		case cm.leak == report.StatusFail && (bm.leak == report.StatusPass || bm.leak == report.StatusWarn):
			st, note = report.DeltaRegressed, fmt.Sprintf("memory_leak %s → fail", bm.leak)
		case !bm.judged || !cm.judged:
			st, note = report.DeltaNA, fmt.Sprintf("not judged: needs %.0f+ min of load after the first %.0f s", cfg.LeakMinLoadS/60, cfg.LeakSettleS)
		case cm.growth-bm.growth > floor && cm.r2 >= cfg.MinR2:
			st = report.DeltaRegressed
		case bm.growth-cm.growth > floor && bm.r2 >= cfg.MinR2:
			st = report.DeltaImproved
		}
		metric(MetricMemoryGrowth, "memory growth", "MiB", bg, cg, st, note)
		if bm.slope != nil && cm.slope != nil {
			st := report.DeltaOK
			d := *cm.slope - *bm.slope
			switch {
			case d > cc.MaxLeakSlopeIncrease && d*cm.minutes >= floor && cm.r2 >= cfg.MinR2:
				st = report.DeltaRegressed
			case -d > cc.MaxLeakSlopeIncrease && -d*bm.minutes >= floor && bm.r2 >= cfg.MinR2:
				st = report.DeltaImproved
			}
			metric(MetricLeakSlope, "leak slope", "MiB/min", bm.slope, cm.slope, st, "")
		}
		if bm.retained != nil || cm.retained != nil {
			st := report.DeltaOK
			if bm.retained != nil && cm.retained != nil {
				st = latencyDelta(*bm.retained, *cm.retained, 0, floor)
			}
			metric(MetricRetained, "retained after cool-down", "MiB", bm.retained, cm.retained, st, "")
		}
	}

	if bc, ccap := base.Capacity, cur.Capacity; bc != nil && ccap != nil {
		v := func(p *int) *float64 {
			if p == nil {
				return report.F(0)
			}
			return report.F(float64(*p))
		}
		bv, cv := v(bc.MaxSustainableVUs), v(ccap.MaxSustainableVUs)
		st, note := report.DeltaOK, ""
		switch {
		case bc.Inconclusive || ccap.Inconclusive:
			note = "a run was inconclusive (load generator saturated); not judged"
		case *cv < *bv:
			st = report.DeltaRegressed
		case *cv > *bv:
			st = report.DeltaImproved
		}
		metric(MetricMaxSustainable, "max sustainable concurrency", "agents", bv, cv, st, note)
	}

	for _, t := range c.Tools {
		c.Regressed = c.Regressed || t.Status == report.DeltaRegressed
	}
	for _, m := range c.Metrics {
		c.Regressed = c.Regressed || m.Status == report.DeltaRegressed
	}
	return c
}

// BaselineName names the baseline for messages: "abc1234 (main)", else "run 1a2b3c4d".
func BaselineName(b report.BaselineRef) string {
	if b.Git != nil && b.Git.SHA != "" {
		s := b.Git.SHA
		if len(s) > 7 {
			s = s[:7]
		}
		if ref := strings.TrimPrefix(strings.TrimPrefix(b.Git.Ref, "refs/heads/"), "refs/"); ref != "" {
			s += " (" + ref + ")"
		}
		return s
	}
	id := b.RunID
	if len(id) > 8 {
		id = id[:8]
	}
	return "run " + id
}

// RegressionVerdict turns a comparison into the regression verdict: fail
// when it regressed (warn when failOnRegression is false), warn when the
// runs differ in a way that may make the comparison unfair, else pass.
func RegressionVerdict(c *report.Comparison, failOnRegression bool) report.Verdict {
	v := report.Verdict{ID: report.VerdictRegression, Signal: "comparison", Status: report.StatusPass}
	name := BaselineName(c.Baseline)
	if c.Regressed {
		v.Status = report.StatusFail
		if !failOnRegression {
			v.Status = report.StatusWarn
		}
		rs := c.Reasons
		more := ""
		if len(rs) > 3 {
			more = fmt.Sprintf(" (+%d more)", len(rs)-3)
			rs = rs[:3]
		}
		v.Message = fmt.Sprintf("Performance regression vs baseline %s: %s%s.", name, strings.Join(rs, "; "), more)
	} else {
		counts := map[string]int{}
		var added, removed []string
		for _, t := range c.Tools {
			counts[t.Status]++
			switch t.Status {
			case report.DeltaAdded:
				added = append(added, "`"+t.Name+"`")
			case report.DeltaRemoved:
				removed = append(removed, "`"+t.Name+"`")
			}
		}
		parts := []string{fmt.Sprintf("%d of %d tools within noise", counts[report.DeltaOK], len(c.Tools))}
		if n := counts[report.DeltaImproved]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d improved", n))
		}
		if n := counts[report.DeltaFewCalls]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d with fewer than %d calls (not judged)", n, c.Rules.MinCalls))
		}
		if len(added) > 0 {
			parts = append(parts, "added "+strings.Join(added, ", "))
		}
		if len(removed) > 0 {
			parts = append(parts, "removed "+strings.Join(removed, ", "))
		}
		v.Message = fmt.Sprintf("No regression vs baseline %s: %s.", name, strings.Join(parts, ", "))
	}
	if len(c.Warnings) > 0 {
		if v.Status == report.StatusPass {
			v.Status = report.StatusWarn
		}
		v.Message += " Note: " + strings.Join(c.Warnings, "; ") + "."
	}
	return v
}
