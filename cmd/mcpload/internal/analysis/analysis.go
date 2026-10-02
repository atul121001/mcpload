// Package analysis computes leak and drift verdicts from a report's time series.
//
// Method (docs/ARCHITECTURE.md §6, report/schema/README.md "Verdict ids"):
//
//   - Regression window: points with WarmupEndS <= t < LoadEndS (constant load).
//     x is minutes since run start, so slopes are in signal-units per minute.
//   - Baseline: mean of the signal over the first BaselineWindowS seconds of the
//     load window (the "post-warm-up baseline"); falls back to the first point.
//   - Cool-down window: points with LoadEndS+CooldownSkipS <= t <= CooldownEndS.
//     If it has no points, recovery is not judged (cooldownRecovered omitted).
//   - Recovery (server leak verdicts only). Let
//     growth   = fitted value at LoadEndS - baseline   (how much the fit rose under load),
//     retained = mean(cool-down) - baseline,
//     tol      = max(CooldownRecoveryFrac * max(growth, 0), 2 * residual std-dev of the fit).
//     The signal recovered iff retained <= tol, i.e. cool-down gave back at
//     least (1 - CooldownRecoveryFrac) = 80% of the growth, with a noise floor
//     so a flat signal is never judged "not recovered" because of jitter.
//   - Leak verdicts (memory_leak, session_leak, fd_leak): fail when
//     (slope > limit AND R² >= MinR2) OR cool-down did not recover; else pass.
//   - Drift verdicts (latency_drift, error_drift): warn when the rise over the
//     load window (slope * load minutes) exceeds LatencyDriftFrac of baseline
//     (latency) or ErrorDriftPts absolute (error rate) AND R² >= DriftMinR2.
//   - skipped when the series is absent (e.g. sampler "none", or docker which
//     only reports RSS) or has fewer than MinPoints points in the load window.
package analysis

import (
	"fmt"
	"math"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

const mib = 1024 * 1024

// Config holds verdict limits.
type Config struct {
	// LeakSlopeMBPerMin: memory_leak slope limit in MiB/min (report slope is bytes/min).
	LeakSlopeMBPerMin float64
	// MinR2: minimum R² for a leak slope to count as a trend.
	MinR2 float64
	// CooldownRecoveryFrac: max fraction of the load-window growth that may be
	// retained (mean of cool-down minus baseline) for the signal to count as recovered.
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
	// MinPoints: minimum points in the load window for a verdict (else skipped).
	MinPoints int
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
		MinPoints:            3,
	}
}

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

// fit is the regression of one signal over the load window plus derived stats.
type fit struct {
	n               int
	slope, icpt, r2 float64 // slope per minute
	baseline        float64
	resStd          float64
	loadMinutes     float64
	growth          float64
	coolN           int
	coolMean        float64
	recovered       *bool
	retained, tol   float64
	loadEndMin      float64
}

func analyze(r *report.Report, ys []*float64, cfg Config, judgeRecovery bool) (fit, bool) {
	var f fit
	t := r.Series.T
	if len(ys) == 0 || len(ys) != len(t) {
		return f, false
	}
	ph := r.Phases
	var xs, vs []float64
	var bSum float64
	var bN int
	for i, ti := range t {
		if ys[i] == nil || !finite(*ys[i]) {
			continue
		}
		if ti >= ph.WarmupEndS && ti < ph.LoadEndS {
			xs = append(xs, ti/60)
			vs = append(vs, *ys[i])
			if ti < ph.WarmupEndS+cfg.BaselineWindowS {
				bSum += *ys[i]
				bN++
			}
		}
	}
	minPts := max(cfg.MinPoints, 2)
	if len(xs) < minPts {
		return f, false
	}
	f.n = len(xs)
	f.slope, f.icpt, f.r2 = LinReg(xs, vs)
	if !finite(f.slope) {
		return f, false
	}
	if bN > 0 {
		f.baseline = bSum / float64(bN)
	} else {
		f.baseline = vs[0]
	}
	if f.n > 2 {
		var ss float64
		for i := range xs {
			d := vs[i] - (f.icpt + f.slope*xs[i])
			ss += d * d
		}
		f.resStd = math.Sqrt(ss / float64(f.n-2))
	}
	f.loadMinutes = (ph.LoadEndS - ph.WarmupEndS) / 60
	f.loadEndMin = ph.LoadEndS / 60
	f.growth = f.icpt + f.slope*f.loadEndMin - f.baseline

	if judgeRecovery {
		var cSum float64
		for i, ti := range t {
			if ys[i] == nil || !finite(*ys[i]) {
				continue
			}
			if ti >= ph.LoadEndS && ti >= ph.LoadEndS+cfg.CooldownSkipS && ti <= ph.CooldownEndS {
				cSum += *ys[i]
				f.coolN++
			}
		}
		if f.coolN > 0 {
			f.coolMean = cSum / float64(f.coolN)
			f.retained = f.coolMean - f.baseline
			f.tol = math.Max(cfg.CooldownRecoveryFrac*math.Max(f.growth, 0), 2*f.resStd)
			rec := f.retained <= f.tol
			f.recovered = &rec
		}
	}
	return f, true
}

func round(v float64, d int) float64 {
	p := math.Pow(10, float64(d))
	return math.Round(v*p) / p
}

func skipped(id, signal, why string) report.Verdict {
	return report.Verdict{ID: id, Status: report.StatusSkipped, Signal: signal, Message: "Skipped: " + why + "."}
}

// leak computes a memory/session/fd leak verdict. scale converts signal units
// to display units (e.g. bytes -> MiB); limit is in display units per minute.
func leak(r *report.Report, cfg Config, id, signal string, ys []*float64, missing string, scale, limit float64, what, unit string) report.Verdict {
	if r.Series.Server.Sampler == report.SamplerNone {
		return skipped(id, signal, "no server-side sampler (--sampler none)")
	}
	if len(ys) == 0 {
		return skipped(id, signal, missing)
	}
	f, ok := analyze(r, ys, cfg, true)
	if !ok {
		return skipped(id, signal, fmt.Sprintf("fewer than %d samples in the constant-load window", max(cfg.MinPoints, 2)))
	}
	slopeD := f.slope / scale
	trending := slopeD > limit && f.r2 >= cfg.MinR2
	v := report.Verdict{
		ID:                id,
		Signal:            signal,
		SlopePerMin:       report.F(round(f.slope, 4)),
		R2:                report.F(round(f.r2, 4)),
		Baseline:          report.F(round(f.baseline, 4)),
		CooldownRecovered: f.recovered,
		Status:            report.StatusPass,
	}
	notRecovered := f.recovered != nil && !*f.recovered
	if trending || notRecovered {
		v.Status = report.StatusFail
	}
	num := func(x float64) string {
		if unit == "MiB" {
			return fmt.Sprintf("%.2f %s", x, unit)
		}
		return fmt.Sprintf("%.2f", x)
	}
	lim := fmt.Sprintf("%g", limit)
	if unit != "" {
		lim += " " + unit
	}
	stats := fmt.Sprintf("%s/min, R²=%.2f", num(slopeD), f.r2)
	notBack := fmt.Sprintf("%s above the post-warm-up baseline of %s", num(f.retained/scale), num(f.baseline/scale))
	switch {
	case trending && notRecovered:
		v.Message = fmt.Sprintf("%s grew %s/min (R²=%.2f) under constant load, above the %s/min limit, and did not recover in cool-down (%s).", what, num(slopeD), f.r2, lim, notBack)
	case trending && f.recovered != nil:
		v.Message = fmt.Sprintf("%s grew %s/min (R²=%.2f) under constant load, above the %s/min limit; it returned near baseline in cool-down, but the slope alone fails the check.", what, num(slopeD), f.r2, lim)
	case trending:
		v.Message = fmt.Sprintf("%s grew %s/min (R²=%.2f) under constant load, above the %s/min limit; no cool-down samples to judge recovery.", what, num(slopeD), f.r2, lim)
	case notRecovered:
		v.Message = fmt.Sprintf("%s did not recover in cool-down (%s), although it did not grow linearly under load (%s).", what, notBack, stats)
	case f.recovered != nil:
		v.Message = fmt.Sprintf("%s flat under constant load (%s; limit %s/min) and returned near baseline (%s) in cool-down.", what, stats, lim, num(f.baseline/scale))
	default:
		v.Message = fmt.Sprintf("%s flat under constant load (%s; limit %s/min); no cool-down samples to judge recovery.", what, stats, lim)
	}
	return v
}

func latencyDrift(r *report.Report, cfg Config) report.Verdict {
	const id, signal = report.VerdictLatencyDrift, "client.p95Ms"
	f, ok := analyze(r, r.Series.Client.P95Ms, cfg, false)
	if !ok {
		return skipped(id, signal, "not enough client p95 samples in the constant-load window")
	}
	v := report.Verdict{ID: id, Signal: signal, Status: report.StatusPass,
		SlopePerMin: report.F(round(f.slope, 4)), R2: report.F(round(f.r2, 4)), Baseline: report.F(round(f.baseline, 3))}
	rise := f.slope * f.loadMinutes
	if f.baseline <= 0 {
		v.Message = fmt.Sprintf("Client p95 slope %.2f ms/min (R²=%.2f); baseline is zero so drift is not judged.", f.slope, f.r2)
		return v
	}
	frac := rise / f.baseline
	if frac > cfg.LatencyDriftFrac && f.r2 >= cfg.DriftMinR2 {
		v.Status = report.StatusWarn
		v.Message = fmt.Sprintf("Client p95 drifted %+.1f%% over the load window (%.1f ms/min, R²=%.2f), above the %.0f%% limit; consistent with growing GC or queueing pressure.", frac*100, f.slope, f.r2, cfg.LatencyDriftFrac*100)
	} else {
		v.Message = fmt.Sprintf("Client p95 stable (%+.1f%% over the load window, %.2f ms/min, R²=%.2f).", frac*100, f.slope, f.r2)
	}
	return v
}

func errorDrift(r *report.Report, cfg Config) report.Verdict {
	const id, signal = report.VerdictErrorDrift, "client.errorRate"
	f, ok := analyze(r, r.Series.Client.ErrorRate, cfg, false)
	if !ok {
		return skipped(id, signal, "not enough client error-rate samples in the constant-load window")
	}
	v := report.Verdict{ID: id, Signal: signal, Status: report.StatusPass,
		SlopePerMin: report.F(round(f.slope, 8)), R2: report.F(round(f.r2, 4)), Baseline: report.F(round(f.baseline, 6))}
	rise := f.slope * f.loadMinutes
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
// threshold or session_not_found verdicts; see ThresholdVerdict and
// SessionNotFoundVerdict.
func Verdicts(r *report.Report, cfg Config) []report.Verdict {
	s := r.Series.Server
	kind := s.Sampler
	return []report.Verdict{
		leak(r, cfg, report.VerdictMemoryLeak, "server.rssBytes", s.RSSBytes,
			fmt.Sprintf("the %s sampler reported no RSS samples", kind), mib, cfg.LeakSlopeMBPerMin, "RSS", "MiB"),
		leak(r, cfg, report.VerdictSessionLeak, "server.activeSessions", s.ActiveSessions,
			fmt.Sprintf("active sessions not reported by the %s sampler", kind), 1, cfg.SessionSlopePerMin, "Active sessions", ""),
		leak(r, cfg, report.VerdictFDLeak, "server.openFds", s.OpenFDs,
			fmt.Sprintf("open file descriptors not reported by the %s sampler", kind), 1, cfg.FDSlopePerMin, "Open file descriptors", ""),
		latencyDrift(r, cfg),
		errorDrift(r, cfg),
	}
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
// (used only for the message; may be 0).
func SessionNotFoundVerdict(count, total float64) report.Verdict {
	v := report.Verdict{ID: report.VerdictSessionNotFound, Signal: "mcp_errors{error_type:session_not_found}", Status: report.StatusPass}
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
