package analysis

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

// Long-lived sessions, recovery after a restart and call integrity
// (report/schema/README.md "Verdict ids").
//
// # session_survival (scenario long-lived)
//
//   - fail when any session died before its planned end (session_not_found, or
//     DEAD_ROUNDS rounds of transport errors in a row); the message names the
//     causes and the median lifetime of the sessions that died.
//   - warn when, for any tool with at least SessionDriftMinCalls successful
//     calls in both the first and the last third of the sessions, late p95 >=
//     SessionDriftRatio x early p95 AND at least SessionDriftMinMs higher.
//   - pass otherwise; skipped when no long-lived session ended.
//
// # recovery (--chaos-restart)
//
//   - Recovery point: the first moment r >= the restart such that requests are
//     served without errors at r and the window
//     [r, r+WindowS) has requests, an error rate (tool_iserror left out) below
//     ErrRate and, when it has successful connects, a connect p95 below
//     ConnectP95Ms. Searched in FineBinS steps.
//   - pass when recovered within BudgetS of the restart; fail when later or
//     never; warn when docker restart itself failed; skipped when the run
//     ended before the restart.
//
// # call_integrity (call ids in params._meta, --calls-url)
//
//   - fail when the server ran any call id more than once; warn when calls
//     failed on the client but ran on the server (the client cannot tell them
//     from calls that never ran, so retrying them would run them twice);
//     pass otherwise. skipped without call ids or without a server-side record.
const (
	SessionDriftRatio    = 1.5
	SessionDriftMinMs    = 25.0
	SessionDriftMinCalls = 30

	// RecoveryFineBinS is the resolution of the recovery search (k6run.FineBin).
	RecoveryFineBinS = 0.1
)

// SessionEnd is one long-lived session as it ended.
type SessionEnd struct {
	LifetimeS float64
	Died      bool
	Cause     string
}

// LongSessions summarises long-lived sessions for report.sessions. early and
// late are per-tool p95 of successful calls in the first and last third of
// the sessions. Returns nil when no session ended.
func LongSessions(ends []SessionEnd, reconnects int64, early, late map[string]PhaseP95) *report.Sessions {
	if len(ends) == 0 {
		return nil
	}
	s := &report.Sessions{Total: int64(len(ends)), DiedByCause: map[string]int64{}, Reconnects: reconnects}
	var all, died []float64
	for _, e := range ends {
		all = append(all, e.LifetimeS)
		if e.Died {
			s.Died++
			c := e.Cause
			if c == "" {
				c = "unknown"
			}
			s.DiedByCause[c]++
			died = append(died, e.LifetimeS)
		} else {
			s.Survived++
		}
	}
	s.LifetimeP50S = round(median(all), 3)
	if len(died) > 0 {
		s.DiedAfterS = report.F(round(median(died), 3))
	}
	// The tool whose p95 rose most from early to late (by ratio), among tools with enough calls.
	best := -1.0
	for name, e := range early {
		l, ok := late[name]
		if !ok || e.Calls < SessionDriftMinCalls || l.Calls < SessionDriftMinCalls || e.P95 <= 0 {
			continue
		}
		r := l.P95 / e.P95
		if r > best || (r == best && name < s.DriftTool) {
			best = r
			s.DriftTool, s.EarlyP95Ms, s.LateP95Ms = name, report.F(round(e.P95, 3)), report.F(round(l.P95, 3))
		}
	}
	return s
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// minSec formats seconds as 5m02s (or 42.0s under a minute).
func minSec(s float64) string {
	if s < 60 {
		return fmt.Sprintf("%.1fs", s)
	}
	t := int(math.Round(s))
	return fmt.Sprintf("%dm%02ds", t/60, t%60)
}

// byCount renders a count map as "a 3, b 1" (largest first).
func byCount(m map[string]int64) string {
	keys := make([]string, 0, len(m))
	for k, v := range m {
		if v > 0 {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

// SessionSurvivalVerdict judges report.sessions (nil: skipped).
func SessionSurvivalVerdict(s *report.Sessions) report.Verdict {
	const id, signal = report.VerdictSessionSurvival, "mcp_session_lifetime"
	if s == nil || s.Total == 0 {
		return skipped(id, signal, "no long-lived session ended (scenario long-lived)")
	}
	v := report.Verdict{ID: id, Signal: signal, Status: report.StatusPass}
	drift := ""
	driftWarn := false
	if s.DriftTool != "" && s.EarlyP95Ms != nil && s.LateP95Ms != nil {
		e, l := *s.EarlyP95Ms, *s.LateP95Ms
		driftWarn = l >= SessionDriftRatio*e && l-e >= SessionDriftMinMs
		drift = fmt.Sprintf("%s p95 %.1f ms early in the sessions → %.1f ms late (×%.2f)", s.DriftTool, e, l, l/e)
	}
	if s.Died > 0 {
		v.Status = report.StatusFail
		why := "the server dropped live sessions"
		if s.DiedByCause["session_not_found"] > 0 {
			why += " (idle timeout, a restart, or a replica that doesn't hold the session)"
		}
		v.Message = fmt.Sprintf("%d of %d sessions died after a median %s with %s: %s. %d reconnects followed.", s.Died, s.Total, minSec(*s.DiedAfterS), byCount(s.DiedByCause), why, s.Reconnects)
		if drift != "" {
			v.Message += " Latency: " + drift + "."
		}
		return v
	}
	v.Message = fmt.Sprintf("All %d sessions stayed open for their planned lifetime (median %s).", s.Total, minSec(s.LifetimeP50S))
	switch {
	case driftWarn:
		v.Status = report.StatusWarn
		v.Message += fmt.Sprintf(" But calls got slower as sessions aged: %s, above the ×%.1f and +%.0f ms limits; consistent with per-session state that grows.", drift, SessionDriftRatio, SessionDriftMinMs)
	case drift != "":
		v.Message += " Late calls kept their latency; largest change: " + drift + "."
	default:
		v.Message += fmt.Sprintf(" Too few calls (under %d per tool in the first and last third) to compare late with early latency.", SessionDriftMinCalls)
	}
	return v
}

// FineBin is RecoveryFineBinS seconds of client activity: requests, errors
// other than tool_iserror (by type), connect attempts, failed connects and
// the durations (ms) of successful connects.
type FineBin struct {
	Reqs, Errors           int64
	ByType                 map[string]int64
	Connects, ConnectFails int64
	ConnectMs              []float64
}

// RecoveryConfig holds the recovery budgets.
type RecoveryConfig struct {
	BudgetS, WindowS, ErrRate, ConnectP95Ms float64
}

// ComputeRecovery finds when the client side was healthy again after a
// restart at atS. bins[i] covers [i, i+1) x RecoveryFineBinS from run start;
// reconnects are the times (s) agents got a new session after a break.
func ComputeRecovery(atS float64, bins []FineBin, reconnects []float64, cfg RecoveryConfig) report.Recovery {
	rc := report.Recovery{BudgetS: cfg.BudgetS, WindowS: cfg.WindowS, ErrorRate: cfg.ErrRate, ConnectP95Ms: cfg.ConnectP95Ms, ErrorsByType: map[string]int64{}}
	start := int(math.Floor(atS/RecoveryFineBinS + 1e-9))
	if start < 0 {
		start = 0
	}
	wb := int(math.Max(1, math.Round(cfg.WindowS/RecoveryFineBinS)))
	healthy := func(from int) bool {
		// The window must open with requests served, so recovery is never
		// dated before the server is back.
		if bins[from].Reqs == 0 || bins[from].Errors > 0 {
			return false
		}
		var reqs, errs int64
		var conn []float64
		for i := from; i < from+wb; i++ {
			reqs += bins[i].Reqs
			errs += bins[i].Errors
			conn = append(conn, bins[i].ConnectMs...)
		}
		if reqs == 0 || float64(errs)/float64(reqs) >= cfg.ErrRate {
			return false
		}
		if len(conn) > 0 {
			sort.Float64s(conn)
			if percentile(conn, 0.95) >= cfg.ConnectP95Ms {
				return false
			}
		}
		return true
	}
	end := len(bins)
	for r := start; r+wb <= len(bins); r++ {
		if healthy(r) {
			rc.Recovered = true
			rc.RecoveryS = report.F(round(math.Max(0, float64(r)*RecoveryFineBinS-atS), 3))
			end = min(len(bins), r+wb)
			break
		}
	}
	firstErr := -1
	for i := start; i < end; i++ {
		b := bins[i]
		rc.ConnectAttempts += b.Connects
		rc.ConnectFailures += b.ConnectFails
		for k, n := range b.ByType {
			rc.ErrorsByType[k] += n
		}
		if firstErr < 0 && b.Errors > 0 {
			firstErr = i
		}
		if firstErr >= 0 && rc.ServerBackS == nil && len(b.ConnectMs) > 0 {
			rc.ServerBackS = report.F(round(math.Max(0, float64(i)*RecoveryFineBinS-atS), 3))
		}
	}
	// Requests and connects land in neighbouring buckets; never date recovery before the server was back.
	if rc.RecoveryS != nil && rc.ServerBackS != nil && *rc.RecoveryS < *rc.ServerBackS {
		rc.RecoveryS = report.F(*rc.ServerBackS)
	}
	for _, t := range reconnects {
		if t >= atS {
			rc.Reconnects++
			rc.LastReconnectS = report.F(round(t-atS, 3))
		}
	}
	return rc
}

// percentile of sorted values with linear interpolation (as k6 does).
func percentile(s []float64, p float64) float64 {
	if len(s) == 0 {
		return 0
	}
	x := p * float64(len(s)-1)
	i := int(x)
	if i >= len(s)-1 {
		return s[len(s)-1]
	}
	return s[i] + (x-float64(i))*(s[i+1]-s[i])
}

// RecoveryVerdict judges report.chaos (nil: skipped).
func RecoveryVerdict(ch *report.Chaos) report.Verdict {
	const id, signal = report.VerdictRecovery, "chaos.recovery"
	switch {
	case ch == nil:
		return skipped(id, signal, "no restart was injected (use --chaos-restart)")
	case ch.Error != "":
		return report.Verdict{ID: id, Signal: signal, Status: report.StatusWarn,
			Message: fmt.Sprintf("docker restart %s at %.0f s failed, so recovery was not measured: %s", ch.Container, ch.AtS, ch.Error)}
	case !ch.Ran:
		return skipped(id, signal, fmt.Sprintf("the run ended before the planned restart of %s", ch.Container))
	case ch.Recovery == nil:
		return skipped(id, signal, "no client samples after the restart")
	}
	rc := ch.Recovery
	v := report.Verdict{ID: id, Signal: signal, Status: report.StatusPass}
	var b strings.Builder
	fmt.Fprintf(&b, "After the restart of %s at %.0f s (docker restart took %.1f s)", ch.Container, ch.AtS, ch.DurationS)
	if rc.ServerBackS != nil {
		fmt.Fprintf(&b, ", the server accepted sessions again after %.1f s", *rc.ServerBackS)
	}
	if rc.Reconnects > 0 && rc.LastReconnectS != nil {
		fmt.Fprintf(&b, " and %d agents reconnected within %.1f s", rc.Reconnects, *rc.LastReconnectS)
	}
	fmt.Fprintf(&b, " (%d connect attempts, %d failed)", rc.ConnectAttempts, rc.ConnectFailures)
	if rc.Recovered {
		fmt.Fprintf(&b, "; errors returned to under %g%% with connect p95 under %.0f ms %.1f s after the restart (budget %.0f s)", rc.ErrorRate*100, rc.ConnectP95Ms, *rc.RecoveryS, rc.BudgetS)
		if *rc.RecoveryS > rc.BudgetS {
			v.Status = report.StatusFail
			b.WriteString(", too slow")
		}
	} else {
		v.Status = report.StatusFail
		fmt.Fprintf(&b, "; errors never stayed under %g%% (with connect p95 under %.0f ms) for %.0f s before the run ended (budget %.0f s)", rc.ErrorRate*100, rc.ConnectP95Ms, rc.WindowS, rc.BudgetS)
	}
	b.WriteString(".")
	if s := byCount(rc.ErrorsByType); s != "" {
		b.WriteString(" Errors during the outage: " + s + ".")
	}
	v.Message = b.String()
	return v
}

// IntegrityInput is what the client and the server saw of tagged calls.
type IntegrityInput struct {
	Source string
	Tagged int64
	// Attempts: per call id that failed on the client or was sent more than
	// once, the outcome of each attempt ("answered" or an error type).
	Attempts map[string][]string
	// Executions: per call id, how often the server ran it.
	Executions map[string]int64
}

// CallIntegrity compares client attempts with server executions.
func CallIntegrity(in IntegrityInput) *report.CallIntegrity {
	ci := &report.CallIntegrity{Source: in.Source, Tagged: in.Tagged}
	for id, outs := range in.Attempts {
		if len(outs) > 1 {
			ci.Retried++
		}
		answered := false
		for _, o := range outs {
			if o == "answered" {
				answered = true
			}
		}
		if !answered {
			ci.ClientFailed++
			if in.Executions[id] > 0 {
				ci.FailedButExecuted++
			} else {
				ci.NeverRan++
			}
		}
	}
	for id, n := range in.Executions {
		if n <= 0 {
			continue
		}
		ci.Executed++
		ci.Executions += n
		if n > 1 {
			ci.Duplicated++
			ci.DuplicateExecutions += n - 1
			if len(in.Attempts[id]) > 1 {
				ci.DuplicatedAfterRetry++
			}
		}
	}
	return ci
}

// CallIntegrityVerdict judges report.callIntegrity. tagged is the number of
// call ids the client sent; ci is nil when the server's executions are unknown.
func CallIntegrityVerdict(tagged int64, ci *report.CallIntegrity) report.Verdict {
	const id, signal = report.VerdictCallIntegrity, "callIntegrity"
	if tagged == 0 && ci == nil {
		return skipped(id, signal, "no tools/call carried a call id (scenario reconnect-storm sends them)")
	}
	if ci == nil {
		return skipped(id, signal, fmt.Sprintf("server-side executions not measured for %d tagged calls; record params._meta[\"io.mcpload/callId\"] on the server (demo: TRACK_CALLS=1) and pass --calls-url", tagged))
	}
	v := report.Verdict{ID: id, Signal: signal, Status: report.StatusPass}
	summary := fmt.Sprintf("%d call ids sent (%d sent more than once); the server ran %d of them, %d executions in all. %d failed on the client: %d of those ran on the server anyway, %d never ran.",
		ci.Tagged, ci.Retried, ci.Executed, ci.Executions, ci.ClientFailed, ci.FailedButExecuted, ci.NeverRan)
	switch {
	case ci.Duplicated > 0:
		v.Status = report.StatusFail
		v.Message = fmt.Sprintf("%d calls ran more than once on the server (%d extra executions; %d of them were re-sent by the client with the same call id, %d were not): a non-idempotent tool would have acted twice. ",
			ci.Duplicated, ci.DuplicateExecutions, ci.DuplicatedAfterRetry, ci.Duplicated-ci.DuplicatedAfterRetry) + summary
	case ci.FailedButExecuted > 0:
		v.Status = report.StatusWarn
		v.Message = fmt.Sprintf("%d calls failed on the client but ran on the server: the client cannot tell them from calls that never ran, so retrying them runs them twice unless the server deduplicates by call id. ", ci.FailedButExecuted) + summary
	default:
		v.Message = "No call ran twice and every call the server ran was answered. " + summary
	}
	return v
}
