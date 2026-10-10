// Package analysis computes leak, drift and load-generator verdicts from a
// report's time series and totals.
//
// Method (docs/ARCHITECTURE.md §6, report/schema/README.md "Verdict ids"):
//
// # Windows
//
//   - Load window: points with WarmupEndS <= t < LoadEndS (constant load).
//     x is minutes since run start, so slopes are in signal-units per minute.
//   - Leak window: the load window, but never starting before LeakSettleS
//     (60 s) into the run, so that startup growth of a scenario without a
//     warm-up phase is not mistaken for a leak.
//   - Baseline: mean of the raw signal over the first BaselineWindowS seconds of
//     the (leak or load) window; falls back to the first point.
//   - Cool-down window: points with LoadEndS+CooldownSkipS <= t <= CooldownEndS.
//
// # Leak verdicts (memory_leak, session_leak, fd_leak)
//
//   - skipped when the sampler is "none", the series is absent, the run has no
//     cool-down (CooldownEndS <= LoadEndS) or the leak window is shorter than
//     LeakMinLoadS (2 min): such runs cannot tell a leak from startup growth.
//     When the leak window is shorter than LeakSoakLoadS (10 min) the verdict is
//     still judged, but the message says only fast leaks (≳2 MiB/min) are
//     reliably detected.
//   - Fit: OLS over the leak window. For memory (RSS, heap) the fit is made on
//     the lower envelope of the series (a centred rolling minimum over
//     EnvelopeBuckets buckets), which removes most of the GC sawtooth without
//     shifting the slope of a real trend.
//   - Trend: slope > limit AND R² >= MinR2 AND fitted growth over the leak
//     window (slope × minutes) >= a growth floor (5 MiB for memory, 3 sessions,
//     5 fds). A steep slope with low R² is reported as "no consistent trend".
//   - Recovery, memory: retained = mean(cool-down) − baseline;
//     tol = max(CooldownRecoveryFrac × max(growth, 0), 2 × residual std-dev of
//     the raw fit); recovered iff retained <= tol. Runtimes rarely give memory
//     back right away, so not recovering only fails when retained exceeds
//     allowed = clamp(limit × leak-window minutes, growth floor, MemRetainCapMiB),
//     i.e. between 5 and 10 MiB with the defaults. The cap keeps long soaks from
//     tolerating an arbitrarily large residue.
//   - Recovery, sessions and fds: cool-down is compared to the idle level, not
//     to the under-load baseline (which would hide a small leak behind the
//     sessions/fds that are legitimately open under load). idle = minimum
//     sample before load (t < WarmupEndS); without a warm-up phase it is 0 for
//     sessions and the first sample for fds. retained = min(cool-down) − idle;
//     recovered iff retained <= SessionRetainMax (2) / FDRetainMax (5).
//   - Status: fail when trending OR (not recovered AND, for memory, retained >
//     allowed); else pass. memory_leak judges RSS and, when present, heapBytes;
//     it fails if either fails and reports the failing signal.
//
// # Drift verdicts
//
//   - latency_drift: computed per tool on series.tools[name].p95Ms when present
//     (falling back to client.p95Ms), so a mixed-tool p95 cannot hide one tool
//     regressing. warn when, for any tool, the rise over the load window
//     (slope × load minutes) exceeds LatencyDriftFrac of its baseline AND
//     at least LatencyDriftMinMs in absolute terms AND R² >= DriftMinR2.
//     The absolute floor stops fast tools (a few ms) from warning on noise.
//   - error_drift: warn when the error-rate rise over the load window exceeds
//     ErrorDriftPts (absolute) AND R² >= DriftMinR2.
//   - Both drift verdicts are skipped when the load window is shorter than
//     DriftMinLoadS: a few buckets can't separate a trend from noise.
//
// # Generator verdict
//
//   - GeneratorVerdict: dropped/(iterations+dropped) > 1% warn, > 10% fail;
//     run.generator.cpuMaxPct (busiest sampling interval; the average spans
//     the idle cool-down too) > 90 warn; skipped when neither is known.
//
// Every verdict is skipped when its series has fewer than MinPoints points in
// its window.
package analysis

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

const mib = 1024 * 1024

// Config holds verdict limits.
type Config struct {
	// LeakSlopeMBPerMin: memory_leak slope limit in MiB/min (report slope is bytes/min).
	LeakSlopeMBPerMin float64
	// MinR2: minimum R² for a leak slope to count as a trend.
	MinR2 float64
	// CooldownRecoveryFrac: max fraction of the load-window growth that memory
	// may retain (mean of cool-down minus baseline) to count as recovered.
	CooldownRecoveryFrac float64
	// CooldownSkipS: seconds after LoadEndS ignored before judging recovery.
	CooldownSkipS float64
	// SessionSlopePerMin: session_leak slope limit (sessions/min).
	SessionSlopePerMin float64
	// FDSlopePerMin: fd_leak slope limit (fds/min).
	FDSlopePerMin float64
	// LatencyDriftFrac: warn when p95 rises by more than this fraction of baseline over the load window.
	LatencyDriftFrac float64
	// ErrorDriftPts: warn when error rate rises by more than this (absolute, 0..1) over the load window.
	ErrorDriftPts float64
	// BaselineWindowS: length of the post-warm-up baseline window, seconds.
	BaselineWindowS float64
	// DriftMinR2: minimum R² for drift verdicts to warn.
	DriftMinR2 float64
	// LatencyDriftMinMs: minimum absolute p95 rise (ms) over the load window
	// for latency_drift to warn.
	LatencyDriftMinMs float64
	// DriftMinLoadS: drift verdicts are skipped when the load window is shorter (seconds).
	DriftMinLoadS float64
	// MinPoints: minimum points in the window for a verdict (else skipped).
	MinPoints int

	// LeakMinLoadS: leak verdicts are skipped when the leak window is shorter (seconds).
	LeakMinLoadS float64
	// LeakSoakLoadS: below this leak-window length (seconds) the message notes
	// that only fast leaks are reliably detected.
	LeakSoakLoadS float64
	// LeakSettleS: the leak window never starts before this many seconds into the run.
	LeakSettleS float64
	// EnvelopeBuckets: width of the centred rolling minimum used to fit memory
	// series (1 or less disables it).
	EnvelopeBuckets int
	// MemMinGrowthMiB: minimum fitted RSS/heap growth over the leak window for a trend.
	MemMinGrowthMiB float64
	// MemRetainCapMiB: cap on the memory residue allowed after cool-down.
	MemRetainCapMiB float64
	// SessionMinGrowth: minimum fitted session growth over the leak window for a trend.
	SessionMinGrowth float64
	// FDMinGrowth: minimum fitted fd growth over the leak window for a trend.
	FDMinGrowth float64
	// SessionRetainMax: sessions still open in cool-down above the idle level that fail session_leak.
	SessionRetainMax float64
	// FDRetainMax: fds still open in cool-down above the idle level that fail fd_leak.
	FDRetainMax float64
}

// DefaultConfig returns the default limits.
func DefaultConfig() Config {
	return Config{
		LeakSlopeMBPerMin:    1.0,
		MinR2:                0.7,
		CooldownRecoveryFrac: 0.2,
		CooldownSkipS:        30,
		SessionSlopePerMin:   0.5,
		FDSlopePerMin:        1,
		LatencyDriftFrac:     0.5,
		ErrorDriftPts:        0.01,
		BaselineWindowS:      120,
		DriftMinR2:           0.5,
		LatencyDriftMinMs:    25,
		DriftMinLoadS:        120,
		MinPoints:            3,
		LeakMinLoadS:         120,
		LeakSoakLoadS:        600,
		LeakSettleS:          60,
		EnvelopeBuckets:      3,
		MemMinGrowthMiB:      5,
		MemRetainCapMiB:      10,
		SessionMinGrowth:     3,
		FDMinGrowth:          5,
		SessionRetainMax:     2,
		FDRetainMax:          5,
	}
}

// Generator verdict limits.
const (
	GeneratorDroppedWarnFrac = 0.01
	GeneratorDroppedFailFrac = 0.10
	GeneratorCPUWarnPct      = 90.0 // applied to run.generator.cpuMaxPct
)

// LinReg is ordinary least squares of y on x. Pairs where either value is NaN
// (or ±Inf) are skipped; extra elements of the longer slice are ignored.
// r2 is the coefficient of determination (0 when y is constant). With fewer
// than 2 usable points or constant x, it returns NaN for all three.
func LinReg(x, y []float64) (slope, intercept, r2 float64) {
	n := min(len(x), len(y))
	var sx, sy float64
	var k int
	ok := func(i int) bool { return finite(x[i]) && finite(y[i]) }
	for i := 0; i < n; i++ {
		if ok(i) {
			sx += x[i]
			sy += y[i]
			k++
		}
	}
	if k < 2 {
		return math.NaN(), math.NaN(), math.NaN()
	}
	mx, my := sx/float64(k), sy/float64(k)
	var sxx, sxy, syy float64
	for i := 0; i < n; i++ {
		if ok(i) {
			dx, dy := x[i]-mx, y[i]-my
			sxx += dx * dx
			sxy += dx * dy
			syy += dy * dy
		}
	}
	if sxx == 0 {
		return math.NaN(), math.NaN(), math.NaN()
	}
	slope = sxy / sxx
	intercept = my - slope*mx
	if syy == 0 {
		return slope, intercept, 0
	}
	r2 = (sxy * sxy) / (sxx * syy)
	return slope, intercept, math.Min(1, math.Max(0, r2))
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// rollingMin returns the centred rolling minimum of vs over w points.
func rollingMin(vs []float64, w int) []float64 {
	if w <= 1 {
		return vs
	}
	half := w / 2
	out := make([]float64, len(vs))
	for i := range vs {
		m := vs[i]
		for j := max(0, i-half); j <= min(len(vs)-1, i+half); j++ {
			m = math.Min(m, vs[j])
		}
		out[i] = m
	}
	return out
}

// points returns (t/60, value) for non-missing samples with from <= t < to
// (or t <= to when inclusive).
func points(t []float64, ys []*float64, from, to float64, inclusive bool) (xs, vs []float64) {
	for i, ti := range t {
		if i >= len(ys) || ys[i] == nil || !finite(*ys[i]) {
			continue
		}
		if ti >= from && (ti < to || (inclusive && ti <= to)) {
			xs = append(xs, ti/60)
			vs = append(vs, *ys[i])
		}
	}
	return xs, vs
}

// fit is the regression of one signal over a window plus derived stats.
type fit struct {
	n               int
	slope, icpt, r2 float64 // slope per minute (of the envelope when used)
	baseline        float64 // mean of raw values in the first BaselineWindowS
	rawResStd       float64 // residual std-dev of the raw OLS fit
	minutes         float64 // window length
}

func fitWindow(r *report.Report, ys []*float64, cfg Config, from float64, envelope int) (fit, bool) {
	var f fit
	t := r.Series.T
	if len(ys) == 0 || len(ys) != len(t) {
		return f, false
	}
	to := r.Phases.LoadEndS
	xs, vs := points(t, ys, from, to, false)
	if len(xs) < max(cfg.MinPoints, 2) {
		return f, false
	}
	f.n = len(xs)
	f.slope, f.icpt, f.r2 = LinReg(xs, rollingMin(vs, envelope))
	if !finite(f.slope) {
		return f, false
	}
	var bSum float64
	var bN int
	for i, x := range xs {
		if x*60 < from+cfg.BaselineWindowS {
			bSum += vs[i]
			bN++
		}
	}
	if bN > 0 {
		f.baseline = bSum / float64(bN)
	} else {
		f.baseline = vs[0]
	}
	if f.n > 2 {
		rs, ri, _ := LinReg(xs, vs)
		var ss float64
		for i := range xs {
			d := vs[i] - (ri + rs*xs[i])
			ss += d * d
		}
		f.rawResStd = math.Sqrt(ss / float64(f.n-2))
	}
	f.minutes = (to - from) / 60
	return f, true
}

func round(v float64, d int) float64 {
	p := math.Pow(10, float64(d))
	return math.Round(v*p) / p
}

func skipped(id, signal, why string) report.Verdict {
	return report.Verdict{ID: id, Status: report.StatusSkipped, Signal: signal, Message: "Skipped: " + why + "."}
}

// ProtocolStateless is the first stateless MCP protocol revision. It mirrors
// ProtocolStateless in xk6-mcpload/client/types.go (a separate Go module that
// cmd/mcpload does not import).
const ProtocolStateless = "2026-07-28"

// IsStateless reports whether a run's protocol version uses the stateless
// (2026-07-28+) transport, which has no sessions. It mirrors IsStateless in
// xk6-mcpload/client/types.go; in addition it returns false for anything that
// is not a YYYY-MM-DD date ("auto", "unknown", ""), which plain string
// comparison would otherwise rank after "2026-07-28".
func IsStateless(protocol string) bool {
	if len(protocol) != len("2006-01-02") {
		return false
	}
	if _, err := time.Parse("2006-01-02", protocol); err != nil {
		return false
	}
	return protocol >= ProtocolStateless
}

// statelessSkipped is the verdict for a session verdict on a stateless run:
// a pass there would be vacuous, since no session can be lost.
func statelessSkipped(id, signal string) report.Verdict {
	return skipped(id, signal, "the stateless protocol ("+ProtocolStateless+") has no sessions")
}

// leakSpec describes one leak signal.
type leakSpec struct {
	id, signal string
	what       string  // "RSS", "Active sessions", ...
	unit       string  // "MiB" or ""
	scale      float64 // signal units per display unit
	limit      float64 // display units per minute
	floor      float64 // minimum fitted growth over the window, display units
	memory     bool    // memory rules (envelope, baseline residue) vs counter rules (idle residue)
	retainMax  float64 // counters: allowed residue above idle, display units
	idleZero   bool    // counters without warm-up: idle level is 0 (sessions) instead of the first sample (fds)
}

func (s leakSpec) num(x float64) string {
	if math.Abs(x) < 0.005 {
		x = 0 // avoid "-0.00"
	}
	if s.unit != "" {
		return fmt.Sprintf("%.2f %s", x, s.unit)
	}
	return fmt.Sprintf("%.2f", x)
}

func (s leakSpec) cnt(x float64) string {
	if s.unit != "" {
		return s.num(x)
	}
	return fmt.Sprintf("%g", round(x, 2))
}

// leakResult is one judged leak signal.
type leakResult struct {
	v    report.Verdict
	fail bool
	body string // message without the short-run note
}

// leakWindow returns the start of the leak window and whether leak verdicts
// can be judged at all (why is the skip reason otherwise).
func leakWindow(r *report.Report, cfg Config) (from float64, why string) {
	ph := r.Phases
	from = math.Max(ph.WarmupEndS, cfg.LeakSettleS)
	if ph.CooldownEndS <= ph.LoadEndS {
		return from, "needs a soak run with cool-down (this run has no cool-down phase, so startup growth cannot be told from a leak)"
	}
	if ph.LoadEndS-from < cfg.LeakMinLoadS {
		return from, fmt.Sprintf("needs a soak run with cool-down (only %.0f s of constant load to judge; at least %.0f s needed)", math.Max(ph.LoadEndS-from, 0), cfg.LeakMinLoadS)
	}
	return from, ""
}

func judgeLeak(r *report.Report, cfg Config, s leakSpec, ys []*float64, from float64) (leakResult, bool) {
	env := 0
	if s.memory {
		env = cfg.EnvelopeBuckets
	}
	f, ok := fitWindow(r, ys, cfg, from, env)
	if !ok {
		return leakResult{}, false
	}
	ph := r.Phases
	slopeD := f.slope / s.scale
	growthD := slopeD * f.minutes
	steep := slopeD > s.limit
	consistent := f.r2 >= cfg.MinR2
	bigEnough := growthD >= s.floor
	trending := steep && consistent && bigEnough

	// Recovery.
	_, cool := points(r.Series.T, ys, ph.LoadEndS+cfg.CooldownSkipS, ph.CooldownEndS, true)
	var recovered *bool
	var notRecoveredFail bool
	var recMsg string
	if len(cool) == 0 {
		recMsg = "no cool-down samples to judge recovery"
	} else if s.memory {
		var sum float64
		for _, v := range cool {
			sum += v
		}
		retained := sum/float64(len(cool)) - f.baseline
		tol := math.Max(cfg.CooldownRecoveryFrac*math.Max(f.slope*f.minutes, 0), 2*f.rawResStd)
		rec := retained <= tol
		recovered = &rec
		allowed := math.Min(math.Max(s.limit*f.minutes, s.floor), cfg.MemRetainCapMiB)
		notBack := fmt.Sprintf("%s above the post-warm-up baseline of %s", s.num(retained/s.scale), s.num(f.baseline/s.scale))
		switch {
		case rec:
			recMsg = fmt.Sprintf("returned near baseline (%s) in cool-down", s.num(f.baseline/s.scale))
		case retained/s.scale > allowed:
			notRecoveredFail = true
			recMsg = fmt.Sprintf("did not recover in cool-down (%s, more than the %s allowed)", notBack, s.num(allowed))
		default:
			recMsg = fmt.Sprintf("%s after cool-down, within the %s allowed", notBack, s.num(allowed))
		}
	} else {
		_, pre := points(r.Series.T, ys, 0, ph.WarmupEndS, false)
		var idle float64
		switch {
		case len(pre) > 0:
			idle = pre[0]
			for _, v := range pre {
				idle = math.Min(idle, v)
			}
		case s.idleZero:
			idle = 0
		default:
			_, all := points(r.Series.T, ys, math.Inf(-1), math.Inf(1), false)
			idle = all[0]
		}
		level := cool[0]
		for _, v := range cool {
			level = math.Min(level, v)
		}
		retained := level - idle
		rec := retained/s.scale <= s.retainMax
		recovered = &rec
		if rec {
			recMsg = fmt.Sprintf("fell back to %s in cool-down (idle level %s)", s.cnt(level/s.scale), s.cnt(idle/s.scale))
		} else {
			notRecoveredFail = true
			recMsg = fmt.Sprintf("did not return to the idle level in cool-down: %s still open vs %s idle (%s retained, limit %s)", s.cnt(level/s.scale), s.cnt(idle/s.scale), s.cnt(retained/s.scale), s.cnt(s.retainMax))
		}
	}

	lim := fmt.Sprintf("%g", s.limit)
	if s.unit != "" {
		lim += " " + s.unit
	}
	var trendMsg string
	switch {
	case trending:
		trendMsg = fmt.Sprintf("%s grew %s/min (R²=%.2f, +%s over %.0f min) under constant load, above the %s/min limit", s.what, s.num(slopeD), f.r2, s.num(growthD), f.minutes, lim)
	case steep && !consistent:
		trendMsg = fmt.Sprintf("%s showed no consistent trend under constant load (slope %s/min, R² %.2f below %g)", s.what, s.num(slopeD), f.r2, cfg.MinR2)
	case steep:
		trendMsg = fmt.Sprintf("%s rose %s/min (R²=%.2f) under constant load but only %s in total, below the %s growth floor", s.what, s.num(slopeD), f.r2, s.num(growthD), s.num(s.floor))
	default:
		trendMsg = fmt.Sprintf("%s flat under constant load (%s/min, R²=%.2f; limit %s/min)", s.what, s.num(slopeD), f.r2, lim)
	}
	res := leakResult{fail: trending || notRecoveredFail}
	res.body = trendMsg + "; " + recMsg + "."
	res.v = report.Verdict{
		ID:                s.id,
		Signal:            s.signal,
		SlopePerMin:       report.F(round(f.slope, 4)),
		R2:                report.F(round(f.r2, 4)),
		Baseline:          report.F(round(f.baseline, 4)),
		CooldownRecovered: recovered,
		Status:            report.StatusPass,
	}
	if res.fail {
		res.v.Status = report.StatusFail
	}
	return res, true
}

func shortNote(r *report.Report, cfg Config, from float64, memory bool) string {
	load := r.Phases.LoadEndS - from
	if load >= cfg.LeakSoakLoadS {
		return ""
	}
	rate := ""
	if memory {
		rate = " (≳2 MiB/min)"
	}
	return fmt.Sprintf(" Only %.1f min of constant load: only fast leaks%s are reliably detected; run a soak of %.0f+ min to catch slow ones.", load/60, rate, cfg.LeakSoakLoadS/60)
}

func notEnough(cfg Config) string {
	return fmt.Sprintf("fewer than %d samples in the constant-load window", max(cfg.MinPoints, 2))
}

// leak computes a session/fd leak verdict.
func leak(r *report.Report, cfg Config, s leakSpec, ys []*float64, missing string) report.Verdict {
	if r.Series.Server.Sampler == report.SamplerNone {
		return skipped(s.id, s.signal, "no server-side sampler (--sampler none)")
	}
	if len(ys) == 0 {
		return skipped(s.id, s.signal, missing)
	}
	from, why := leakWindow(r, cfg)
	if why != "" {
		return skipped(s.id, s.signal, why)
	}
	res, ok := judgeLeak(r, cfg, s, ys, from)
	if !ok {
		return skipped(s.id, s.signal, notEnough(cfg))
	}
	res.v.Message = res.body + shortNote(r, cfg, from, s.memory)
	return res.v
}

// memoryLeak judges RSS and (when present) heap; it fails if either fails.
func memoryLeak(r *report.Report, cfg Config) report.Verdict {
	srv := r.Series.Server
	rss := leakSpec{id: report.VerdictMemoryLeak, signal: "server.rssBytes", what: "RSS", unit: "MiB", scale: mib,
		limit: cfg.LeakSlopeMBPerMin, floor: cfg.MemMinGrowthMiB, memory: true}
	heap := rss
	heap.signal, heap.what = "server.heapBytes", "Heap"
	if srv.Sampler == report.SamplerNone {
		return skipped(rss.id, rss.signal, "no server-side sampler (--sampler none)")
	}
	if len(srv.RSSBytes) == 0 && len(srv.HeapBytes) == 0 {
		return skipped(rss.id, rss.signal, fmt.Sprintf("the %s sampler reported no RSS samples", srv.Sampler))
	}
	from, why := leakWindow(r, cfg)
	if why != "" {
		return skipped(rss.id, rss.signal, why)
	}
	rr, rok := judgeLeak(r, cfg, rss, srv.RSSBytes, from)
	hr, hok := judgeLeak(r, cfg, heap, srv.HeapBytes, from)
	note := shortNote(r, cfg, from, true)
	switch {
	case !rok && !hok:
		return skipped(rss.id, rss.signal, notEnough(cfg))
	case !hok:
		rr.v.Message = rr.body + note
		return rr.v
	case !rok:
		hr.v.Message = hr.body + note
		return hr.v
	}
	// Both judged: report the failing signal first (RSS when both or neither fail).
	first, second := rr, hr
	if hr.fail && !rr.fail {
		first, second = hr, rr
	}
	v := first.v
	if first.fail || second.fail {
		v.Status = report.StatusFail
	}
	v.Message = first.body + " " + second.body + note
	return v
}

// drift is one drift fit for latency.
type drift struct {
	name   string
	signal string
	f      fit
	frac   float64
	warn   bool
}

func latencyFit(r *report.Report, cfg Config, name, signal string, ys []*float64) (drift, bool) {
	f, ok := fitWindow(r, ys, cfg, r.Phases.WarmupEndS, 0)
	if !ok || f.baseline <= 0 {
		return drift{}, false
	}
	d := drift{name: name, signal: signal, f: f, frac: f.slope * f.minutes / f.baseline}
	d.warn = d.frac > cfg.LatencyDriftFrac && f.slope*f.minutes >= cfg.LatencyDriftMinMs && f.r2 >= cfg.DriftMinR2
	return d, true
}

func latencyVerdict(d drift) report.Verdict {
	return report.Verdict{ID: report.VerdictLatencyDrift, Signal: d.signal, Status: report.StatusPass,
		SlopePerMin: report.F(round(d.f.slope, 4)), R2: report.F(round(d.f.r2, 4)), Baseline: report.F(round(d.f.baseline, 3))}
}

// driftTooShort reports whether the load window is too short to judge drift.
func driftTooShort(r *report.Report, cfg Config) (bool, string) {
	load := r.Phases.LoadEndS - r.Phases.WarmupEndS
	if load < cfg.DriftMinLoadS {
		return true, fmt.Sprintf("run too short to judge drift (%.0f s of load; at least %.0f s needed)", math.Max(load, 0), cfg.DriftMinLoadS)
	}
	return false, ""
}

func latencyDrift(r *report.Report, cfg Config) report.Verdict {
	const id, signal = report.VerdictLatencyDrift, "client.p95Ms"
	if short, why := driftTooShort(r, cfg); short {
		return skipped(id, signal, why)
	}
	if len(r.Series.Tools) > 0 {
		names := make([]string, 0, len(r.Series.Tools))
		for n := range r.Series.Tools {
			names = append(names, n)
		}
		sort.Strings(names)
		var ds, warns []drift
		for _, n := range names {
			if d, ok := latencyFit(r, cfg, n, "tools."+n+".p95Ms", r.Series.Tools[n].P95Ms); ok {
				ds = append(ds, d)
				if d.warn {
					warns = append(warns, d)
				}
			}
		}
		if len(ds) > 0 {
			byFrac := func(s []drift) {
				sort.SliceStable(s, func(i, j int) bool { return s[i].frac > s[j].frac })
			}
			byFrac(ds)
			byFrac(warns)
			desc := func(d drift) string {
				return fmt.Sprintf("%s %+.1f%% (%.1f ms/min, R²=%.2f)", d.name, d.frac*100, d.f.slope, d.f.r2)
			}
			if len(warns) > 0 {
				v := latencyVerdict(warns[0])
				v.Status = report.StatusWarn
				parts := make([]string, len(warns))
				for i, d := range warns {
					parts[i] = desc(d)
				}
				v.Message = fmt.Sprintf("p95 drifted over the load window for %d of %d tools: %s; above the %.0f%% limit, consistent with growing GC or queueing pressure.", len(warns), len(ds), strings.Join(parts, "; "), cfg.LatencyDriftFrac*100)
				return v
			}
			v := latencyVerdict(ds[0])
			v.Message = fmt.Sprintf("Per-tool p95 stable for all %d tools over the load window; largest drift: %s.", len(ds), desc(ds[0]))
			return v
		}
	}
	d, ok := latencyFit(r, cfg, "", signal, r.Series.Client.P95Ms)
	if !ok {
		f, fok := fitWindow(r, r.Series.Client.P95Ms, cfg, r.Phases.WarmupEndS, 0)
		if fok {
			v := latencyVerdict(drift{signal: signal, f: f})
			v.Message = fmt.Sprintf("Client p95 slope %.2f ms/min (R²=%.2f); baseline is zero so drift is not judged.", f.slope, f.r2)
			return v
		}
		return skipped(id, signal, "not enough client p95 samples in the constant-load window")
	}
	v := latencyVerdict(d)
	if d.warn {
		v.Status = report.StatusWarn
		v.Message = fmt.Sprintf("Client p95 drifted %+.1f%% over the load window (%.1f ms/min, R²=%.2f), above the %.0f%% limit; consistent with growing GC or queueing pressure.", d.frac*100, d.f.slope, d.f.r2, cfg.LatencyDriftFrac*100)
	} else {
		v.Message = fmt.Sprintf("Client p95 stable (%+.1f%% over the load window, %.2f ms/min, R²=%.2f).", d.frac*100, d.f.slope, d.f.r2)
	}
	return v
}

func errorDrift(r *report.Report, cfg Config) report.Verdict {
	const id, signal = report.VerdictErrorDrift, "client.errorRate"
	if short, why := driftTooShort(r, cfg); short {
		return skipped(id, signal, why)
	}
	f, ok := fitWindow(r, r.Series.Client.ErrorRate, cfg, r.Phases.WarmupEndS, 0)
	if !ok {
		return skipped(id, signal, "not enough client error-rate samples in the constant-load window")
	}
	v := report.Verdict{ID: id, Signal: signal, Status: report.StatusPass,
		SlopePerMin: report.F(round(f.slope, 8)), R2: report.F(round(f.r2, 4)), Baseline: report.F(round(f.baseline, 6))}
	rise := f.slope * f.minutes
	if rise > cfg.ErrorDriftPts && f.r2 >= cfg.DriftMinR2 {
		v.Status = report.StatusWarn
		v.Message = fmt.Sprintf("Error rate rose %.2f percentage points over the load window (R²=%.2f), above the %.2f-point limit.", rise*100, f.r2, cfg.ErrorDriftPts*100)
	} else {
		v.Message = fmt.Sprintf("Error rate did not trend upward over the load window (%+.2f points, baseline %.2f%%).", rise*100, f.baseline*100)
	}
	return v
}

// Verdicts computes memory_leak, session_leak, fd_leak, latency_drift and
// error_drift (in that order) from r.Series and r.Phases. It does not add the
// threshold, session_not_found or generator verdicts; see ThresholdVerdict,
// SessionNotFoundVerdict and GeneratorVerdict.
func Verdicts(r *report.Report, cfg Config) []report.Verdict {
	s := r.Series.Server
	kind := s.Sampler
	sess := leakSpec{id: report.VerdictSessionLeak, signal: "server.activeSessions", what: "Active sessions", scale: 1,
		limit: cfg.SessionSlopePerMin, floor: cfg.SessionMinGrowth, retainMax: cfg.SessionRetainMax, idleZero: true}
	fds := leakSpec{id: report.VerdictFDLeak, signal: "server.openFds", what: "Open file descriptors", scale: 1,
		limit: cfg.FDSlopePerMin, floor: cfg.FDMinGrowth, retainMax: cfg.FDRetainMax}
	return []report.Verdict{
		memoryLeak(r, cfg),
		leak(r, cfg, sess, s.ActiveSessions, fmt.Sprintf("active sessions not reported by the %s sampler", kind)),
		leak(r, cfg, fds, s.OpenFDs, fmt.Sprintf("open file descriptors not reported by the %s sampler", kind)),
		latencyDrift(r, cfg),
		errorDrift(r, cfg),
	}
}

// GeneratorVerdict judges whether the load generator kept up with the
// requested load: dropped/(iterations+dropped) above 1% warns and above 10%
// fails; run.generator.cpuMaxPct above 90 warns. It is skipped when neither
// summary.iterations/droppedIterations nor run.generator CPU data is present.
// On stdio runs the message also notes that the server processes share the
// host CPU with k6 (a note only; it does not change the status).
func GeneratorVerdict(r *report.Report) report.Verdict {
	const id = report.VerdictGenerator
	v := report.Verdict{ID: id, Status: report.StatusPass}
	rank := map[string]int{report.StatusPass: 0, report.StatusWarn: 1, report.StatusFail: 2}
	raise := func(st string) {
		if rank[st] > rank[v.Status] {
			v.Status = st
		}
	}
	var msgs []string
	var haveDropped, droppedSignal bool
	sm := r.Summary
	if sm.Iterations != nil && sm.DroppedIterations != nil && *sm.Iterations+*sm.DroppedIterations > 0 {
		it, dr := float64(*sm.Iterations), float64(*sm.DroppedIterations)
		frac := dr / (it + dr)
		haveDropped = true
		switch {
		case frac > GeneratorDroppedFailFrac:
			raise(report.StatusFail)
			droppedSignal = true
			msgs = append(msgs, fmt.Sprintf("Load generator could not keep up: only %.1f%% of planned iterations ran (%.0f dropped of %.0f); results don't reflect the requested load.", 100*it/(it+dr), dr, it+dr))
		case frac > GeneratorDroppedWarnFrac:
			raise(report.StatusWarn)
			droppedSignal = true
			msgs = append(msgs, fmt.Sprintf("Load generator fell behind: %.1f%% of planned iterations were dropped (%.0f of %.0f); the achieved load is below the requested load.", 100*frac, dr, it+dr))
		default:
			msgs = append(msgs, fmt.Sprintf("%.0f of %.0f planned iterations ran (%.2f%% dropped).", it, it+dr, 100*frac))
		}
	}
	if g := r.Run.Generator; g != nil && g.CPUMaxPct != nil && finite(*g.CPUMaxPct) {
		// cpuAvgPct covers the whole k6 lifetime (incl. an idle cool-down), so
		// it understates load-phase CPU; judge the busiest interval instead.
		cores, avg := "", ""
		if g.Cores > 0 {
			cores = fmt.Sprintf(" of %d cores", g.Cores)
		}
		if g.CPUAvgPct != nil && finite(*g.CPUAvgPct) {
			avg = fmt.Sprintf(", ~%.0f%% on average over the run", *g.CPUAvgPct)
		}
		if *g.CPUMaxPct > GeneratorCPUWarnPct {
			raise(report.StatusWarn)
			msgs = append(msgs, fmt.Sprintf("k6 used up to ~%.0f%% of CPU%s%s; latency may include generator overhead — run k6 on a separate machine or more cores.", *g.CPUMaxPct, cores, avg))
		} else {
			msgs = append(msgs, fmt.Sprintf("k6 used up to ~%.0f%% of CPU%s%s.", *g.CPUMaxPct, cores, avg))
		}
	}
	if len(msgs) == 0 {
		v = skipped(id, "summary.droppedIterations", "no iteration counts or load-generator CPU data")
		if r.Run.Target.IsStdio() {
			v.Message += " " + stdioGeneratorNote
		}
		return v
	}
	v.Signal = "run.generator.cpuMaxPct"
	if droppedSignal || (haveDropped && v.Status == report.StatusPass) {
		v.Signal = "summary.droppedIterations"
	}
	if r.Run.Target.IsStdio() {
		// A note, not a failure mode: the status stays what k6's numbers say.
		msgs = append(msgs, stdioGeneratorNote)
	}
	v.Message = strings.Join(msgs, " ")
	return v
}

// ThresholdVerdict summarizes k6 threshold results: fail if any failed.
func ThresholdVerdict(ths []report.Threshold) report.Verdict {
	v := report.Verdict{ID: report.VerdictThreshold, Signal: "thresholds", Status: report.StatusPass}
	var failed []report.Threshold
	for _, t := range ths {
		if !t.Passed {
			failed = append(failed, t)
		}
	}
	switch {
	case len(ths) == 0:
		v.Message = "No k6 thresholds were configured."
	case len(failed) == 0:
		v.Message = fmt.Sprintf("All %d thresholds passed.", len(ths))
	default:
		v.Status = report.StatusFail
		msg := fmt.Sprintf("%d of %d thresholds failed: ", len(failed), len(ths))
		for i, t := range failed {
			if i > 0 {
				msg += "; "
			}
			obs := "null"
			if t.Observed != nil {
				obs = fmt.Sprintf("%g", *t.Observed)
			}
			msg += fmt.Sprintf("%s %s (observed %s)", t.Metric, t.Expr, obs)
		}
		v.Message = msg + "."
	}
	return v
}

// SessionNotFoundVerdict judges mcp_errors{error_type:session_not_found}:
// any occurrence (count > 0) is a fail. total is the total request count
// (used only for the message; may be 0). protocol is the run's protocol
// (report.run.protocol): on a stateless run (IsStateless) with no such error
// the verdict is skipped, because there are no sessions to lose and a pass
// would say nothing; a 404 that did occur is still reported as a fail.
func SessionNotFoundVerdict(protocol string, count, total float64) report.Verdict {
	const id, signal = report.VerdictSessionNotFound, "mcp_errors{error_type:session_not_found}"
	if count <= 0 && IsStateless(protocol) {
		return statelessSkipped(id, signal)
	}
	v := report.Verdict{ID: id, Signal: signal, Status: report.StatusPass}
	if count > 0 {
		v.Status = report.StatusFail
		if total > 0 {
			v.Message = fmt.Sprintf("%.0f of %.0f requests (%.3f%%) got 404 session-not-found: requests for an Mcp-Session-Id reached a replica that does not hold the session (load balancer without sticky sessions or shared session store).", count, total, 100*count/total)
		} else {
			v.Message = fmt.Sprintf("%.0f requests got 404 session-not-found: requests for an Mcp-Session-Id reached a replica that does not hold the session (load balancer without sticky sessions or shared session store).", count)
		}
		return v
	}
	v.Message = "No 404 session-not-found responses."
	return v
}
