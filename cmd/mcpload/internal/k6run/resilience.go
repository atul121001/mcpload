package k6run

import (
	"sort"
	"time"
)

// Custom metrics of scenarios/long-lived.js and scenarios/reconnect-storm.js.
const (
	MetricSessionLifetime   = "mcp_session_lifetime"   // Trend (ms), tags outcome=survived|died, cause
	MetricSessionReconnects = "mcp_session_reconnects" // Counter
	MetricReconnects        = "mcp_reconnects"         // Counter, one per reconnect after a broken session
	MetricSessionBreaks     = "mcp_session_breaks"     // Counter, tag cause
	MetricCallsTagged       = "mcp_calls_tagged"       // Counter, one per call id sent
	MetricCallAttempts      = "mcp_call_attempts"      // Counter, tags call_id, outcome (answered | error type)

	// SessionAgeTag marks calls in the first ("early") and last ("late") third of a long-lived session.
	SessionAgeTag = "session_age"

	// FineBin is the width of the fine-grained buckets used to time recovery after a restart.
	FineBin = 100 * time.Millisecond
)

// fineBin counts requests in one FineBin. errors leave out tool_iserror (a tool's
// own failure says nothing about the server being reachable).
type fineBin struct {
	reqs, errors           float64
	byType                 map[string]float64
	connects, connectFails int
	connect                []float64 // successful connect durations (ms)
}

// resilienceAgg collects what the session_survival, recovery and call_integrity verdicts need.
type resilienceAgg struct {
	fine        []fineBin
	lifetimes   []SessionEnd
	reconnSess  float64
	reconnects  []time.Time
	breaks      map[string]float64
	tagged      float64
	attempts    map[string][]string
	ageTools    map[string]map[string][]float64 // session_age -> tool -> successful durations
	sawSessions bool
}

func (a *Aggregator) fineFor(t time.Time) *fineBin {
	i := 0
	if d := t.Sub(a.Origin); d > 0 {
		i = int(d / FineBin)
	}
	r := &a.res
	for len(r.fine) <= i {
		r.fine = append(r.fine, fineBin{})
	}
	return &r.fine[i]
}

// addResilience folds the samples the resilience verdicts use; called for every point.
func (a *Aggregator) addResilience(metric string, t time.Time, v float64, tags map[string]string) {
	r := &a.res
	switch metric {
	case MetricReqs:
		a.fineFor(t).reqs += v
	case MetricErrors:
		if et := tags["error_type"]; et != ErrorTypeToolIsError {
			b := a.fineFor(t)
			b.errors += v
			if b.byType == nil {
				b.byType = map[string]float64{}
			}
			if et == "" {
				et = "unknown"
			}
			b.byType[et] += v
		}
	case MetricConnectDuration:
		b := a.fineFor(t)
		b.connects++
		if tags["error_type"] != "" {
			b.connectFails++
		} else {
			b.connect = append(b.connect, v)
		}
	case MetricReqDuration:
		age := tags[SessionAgeTag]
		if age == "" || tags["method"] != methodToolsCall || tags["tool"] == "" || tags["error_type"] != "" {
			return
		}
		if r.ageTools == nil {
			r.ageTools = map[string]map[string][]float64{}
		}
		m := r.ageTools[age]
		if m == nil {
			m = map[string][]float64{}
			r.ageTools[age] = m
		}
		m[tags["tool"]] = append(m[tags["tool"]], v)
	case MetricSessionLifetime:
		r.sawSessions = true
		r.lifetimes = append(r.lifetimes, SessionEnd{T: t, LifetimeMs: v, Died: tags["outcome"] == "died", Cause: tags["cause"]})
	case MetricSessionReconnects:
		r.reconnSess += v
	case MetricReconnects:
		for k := 0; k < int(v+0.5); k++ {
			r.reconnects = append(r.reconnects, t)
		}
	case MetricSessionBreaks:
		if r.breaks == nil {
			r.breaks = map[string]float64{}
		}
		r.breaks[sanitizeKey(tags["cause"])] += v
	case MetricCallsTagged:
		r.tagged += v
	case MetricCallAttempts:
		id := tags["call_id"]
		if id == "" {
			return
		}
		if r.attempts == nil {
			r.attempts = map[string][]string{}
		}
		r.attempts[id] = append(r.attempts[id], tags["outcome"])
	}
}

// SessionEnd is one long-lived session as it ended (mcp_session_lifetime).
type SessionEnd struct {
	T          time.Time
	LifetimeMs float64
	Died       bool
	Cause      string
}

// LongSessions returns the ended long-lived sessions and the reconnects that
// followed deaths; ok is false when the run emitted no mcp_session_lifetime.
func (a *Aggregator) LongSessions() (ends []SessionEnd, reconnects int64, ok bool) {
	r := a.res
	return r.lifetimes, round(r.reconnSess), r.sawSessions
}

// SessionAgeTools returns per-tool latency of successful calls tagged
// session_age=<age> (nil when there were none).
func (a *Aggregator) SessionAgeTools(age string) map[string]PhaseTool {
	m := a.res.ageTools[age]
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]PhaseTool, len(m))
	for name, d := range m {
		s := append([]float64(nil), d...)
		sort.Float64s(s)
		out[name] = PhaseTool{Calls: len(s), P50: Percentile(s, 0.50), P95: Percentile(s, 0.95)}
	}
	return out
}

// FineStats is one FineBin of client activity: requests, errors other than
// tool_iserror (by type), connect attempts, failed connects and the durations
// (ms) of successful connects.
type FineStats struct {
	Reqs, Errors           int64
	ByType                 map[string]int64
	Connects, ConnectFails int64
	ConnectMs              []float64
}

// Fine returns the FineBin buckets from the run origin.
func (a *Aggregator) Fine() []FineStats {
	out := make([]FineStats, len(a.res.fine))
	for i, b := range a.res.fine {
		f := FineStats{Reqs: round(b.reqs), Errors: round(b.errors), Connects: int64(b.connects), ConnectFails: int64(b.connectFails), ConnectMs: b.connect}
		if len(b.byType) > 0 {
			f.ByType = make(map[string]int64, len(b.byType))
			for k, v := range b.byType {
				f.ByType[sanitizeKey(k)] += round(v)
			}
		}
		out[i] = f
	}
	return out
}

// Reconnects returns the times of mcp_reconnects samples (seconds since the
// origin) and the session breaks by cause.
func (a *Aggregator) Reconnects() (atS []float64, breaks map[string]int64) {
	for _, t := range a.res.reconnects {
		atS = append(atS, t.Sub(a.Origin).Seconds())
	}
	sort.Float64s(atS)
	breaks = map[string]int64{}
	for k, v := range a.res.breaks {
		breaks[k] = round(v)
	}
	return atS, breaks
}

// CallAttempts returns the number of call ids the client sent
// (mcp_calls_tagged) and, per call id that failed or was sent more than once,
// the outcome of each attempt ("answered" or an error type).
func (a *Aggregator) CallAttempts() (tagged int64, attempts map[string][]string) {
	return round(a.res.tagged), a.res.attempts
}
