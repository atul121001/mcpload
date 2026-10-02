// Package report defines the mcpload report.json (schema v1) data model, its
// semantic validation, JSON I/O, the pass/fail rule and the static HTML renderer.
//
// The structs mirror report/schema/report.v1.json exactly; json tags match the
// schema property names. Optional schema properties are pointers or omitempty.
package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"
)

// SchemaVersion is the major schema version this package writes.
const SchemaVersion = "1"

// Sampler kinds (series.server.sampler).
const (
	SamplerDocker     = "docker"
	SamplerPrometheus = "prometheus"
	SamplerNone       = "none"
)

// Verdict ids.
const (
	VerdictMemoryLeak      = "memory_leak"
	VerdictSessionLeak     = "session_leak"
	VerdictFDLeak          = "fd_leak"
	VerdictLatencyDrift    = "latency_drift"
	VerdictErrorDrift      = "error_drift"
	VerdictSessionNotFound = "session_not_found"
	VerdictThreshold       = "threshold"
)

// Verdict statuses.
const (
	StatusPass    = "pass"
	StatusFail    = "fail"
	StatusWarn    = "warn"
	StatusSkipped = "skipped"
)

// Report is the root of report.json.
type Report struct {
	SchemaVersion    string      `json:"schemaVersion"`
	Tool             ToolInfo    `json:"tool"`
	Run              Run         `json:"run"`
	Phases           Phases      `json:"phases"`
	Summary          Summary     `json:"summary"`
	Tools            []ToolStats `json:"tools"`
	Thresholds       []Threshold `json:"thresholds"`
	Series           Series      `json:"series"`
	Verdicts         []Verdict   `json:"verdicts"`
	PayloadsIncluded bool        `json:"payloadsIncluded"`
}

// ToolInfo identifies the program that wrote the report.
type ToolInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Run holds run metadata.
type Run struct {
	ID        string  `json:"id"`
	StartedAt string  `json:"startedAt"` // RFC 3339 UTC; see FormatTime
	EndedAt   string  `json:"endedAt"`   // RFC 3339 UTC
	DurationS float64 `json:"durationS"`
	Scenario  string  `json:"scenario"`
	Protocol  string  `json:"protocol"` // negotiated protocol, never "auto"
	Target    Target  `json:"target"`
	Git       *Git    `json:"git,omitempty"`
	K6Version string  `json:"k6Version"`
	Load      Load    `json:"load"`
}

// Target is the system under test.
type Target struct {
	URL   string `json:"url"`
	Label string `json:"label,omitempty"`
}

// Git is the revision of the system under test. SHA must match ^[0-9a-f]{7,64}$.
type Git struct {
	SHA string `json:"sha"`
	Ref string `json:"ref,omitempty"`
}

// Load is the load shape as passed to k6.
type Load struct {
	Executor         string   `json:"executor"`
	VUs              *int     `json:"vus,omitempty"`
	MaxVUs           *int     `json:"maxVus,omitempty"`
	ArrivalRate      *float64 `json:"arrivalRate,omitempty"`
	ArrivalTimeUnitS *float64 `json:"arrivalTimeUnitS,omitempty"` // must be > 0 when set
}

// Phases are boundaries in seconds since run start.
// Warm-up [0,WarmupEndS), load [WarmupEndS,LoadEndS), cool-down [LoadEndS,CooldownEndS].
type Phases struct {
	WarmupEndS   float64 `json:"warmupEndS"`
	LoadEndS     float64 `json:"loadEndS"`
	CooldownEndS float64 `json:"cooldownEndS"`
}

// Summary holds run totals. Sum(ByErrorType) must equal Errors.
// ByErrorType keys must match ^[a-z][a-z0-9_]*$.
type Summary struct {
	Reqs        int64            `json:"reqs"`
	Errors      int64            `json:"errors"`
	ErrorRate   float64          `json:"errorRate"`
	ByErrorType map[string]int64 `json:"byErrorType"`
}

// ToolStats are per-tool tools/call statistics. Latencies in ms.
type ToolStats struct {
	Name      string  `json:"name"`
	Reqs      int64   `json:"reqs"`
	Errors    int64   `json:"errors"`
	ErrorRate float64 `json:"errorRate"`
	P50       float64 `json:"p50"`
	P95       float64 `json:"p95"`
	P99       float64 `json:"p99"`
	Max       float64 `json:"max"`
}

// Threshold is one k6 threshold expression result. Observed is null when no samples.
type Threshold struct {
	Metric   string   `json:"metric"`
	Expr     string   `json:"expr"`
	Passed   bool     `json:"passed"`
	Observed *float64 `json:"observed"`
}

// Series are parallel arrays; every array has len(T) entries (nil = missing sample).
type Series struct {
	IntervalS float64      `json:"intervalS"`
	T         []float64    `json:"t"`
	Client    ClientSeries `json:"client"`
	Server    ServerSeries `json:"server"`
}

// ClientSeries are k6-side series.
type ClientSeries struct {
	P95Ms     []*float64 `json:"p95Ms"`
	ErrorRate []*float64 `json:"errorRate"`
	RPS       []*float64 `json:"rps"`
}

// ServerSeries are sampler-side series. RSSBytes is [] when Sampler is "none".
// The optional series are omitted from JSON when nil/empty.
type ServerSeries struct {
	Sampler        string     `json:"sampler"`
	RSSBytes       []*float64 `json:"rssBytes"`
	HeapBytes      []*float64 `json:"heapBytes,omitempty"`
	OpenFDs        []*float64 `json:"openFds,omitempty"`
	ActiveSessions []*float64 `json:"activeSessions,omitempty"`
}

// Verdict is one leak/drift/threshold judgement.
type Verdict struct {
	ID                string   `json:"id"`
	Status            string   `json:"status"`
	Signal            string   `json:"signal"`
	SlopePerMin       *float64 `json:"slopePerMin,omitempty"`
	R2                *float64 `json:"r2,omitempty"` // 0..1
	Baseline          *float64 `json:"baseline,omitempty"`
	CooldownRecovered *bool    `json:"cooldownRecovered,omitempty"`
	Message           string   `json:"message"`
}

// F returns a pointer to v (helper for nullable/optional numbers).
func F(v float64) *float64 { return &v }

// I returns a pointer to v.
func I(v int) *int { return &v }

// B returns a pointer to v.
func B(v bool) *bool { return &v }

// FormatTime formats t as RFC 3339 UTC (second precision), as the schema expects.
func FormatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Normalize replaces nil slices/maps that the schema requires to be present
// with empty ones, fills SchemaVersion/Sampler if empty, and converts
// non-finite numbers (NaN/Inf, which JSON cannot encode) to null / absent.
// WriteJSON, Marshal and RenderHTML call it automatically.
func (r *Report) Normalize() {
	if r.SchemaVersion == "" {
		r.SchemaVersion = SchemaVersion
	}
	if r.Summary.ByErrorType == nil {
		r.Summary.ByErrorType = map[string]int64{}
	}
	if r.Tools == nil {
		r.Tools = []ToolStats{}
	}
	if r.Thresholds == nil {
		r.Thresholds = []Threshold{}
	}
	if r.Verdicts == nil {
		r.Verdicts = []Verdict{}
	}
	s := &r.Series
	if s.T == nil {
		s.T = []float64{}
	}
	if s.Server.Sampler == "" {
		s.Server.Sampler = SamplerNone
	}
	for _, p := range []*[]*float64{&s.Client.P95Ms, &s.Client.ErrorRate, &s.Client.RPS, &s.Server.RSSBytes} {
		if *p == nil {
			*p = []*float64{}
		}
	}
	for _, p := range []*[]*float64{&s.Client.P95Ms, &s.Client.ErrorRate, &s.Client.RPS, &s.Server.RSSBytes, &s.Server.HeapBytes, &s.Server.OpenFDs, &s.Server.ActiveSessions} {
		for i, v := range *p {
			if v != nil && !finite(*v) {
				(*p)[i] = nil
			}
		}
	}
	for i := range r.Thresholds {
		if o := r.Thresholds[i].Observed; o != nil && !finite(*o) {
			r.Thresholds[i].Observed = nil
		}
	}
	for i := range r.Verdicts {
		v := &r.Verdicts[i]
		for _, p := range []**float64{&v.SlopePerMin, &v.R2, &v.Baseline} {
			if *p != nil && !finite(**p) {
				*p = nil
			}
		}
	}
}

// Check performs the semantic checks of report/validate.mjs (the ones JSON
// Schema cannot express) plus a few cheap structural ones (schemaVersion,
// timestamps, sampler/status enums, r2 range). It returns all problems joined, or nil.
func (r *Report) Check() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if r.SchemaVersion != SchemaVersion {
		add("schemaVersion must be %q (got %q)", SchemaVersion, r.SchemaVersion)
	}
	p := r.Phases
	if !(p.WarmupEndS <= p.LoadEndS && p.LoadEndS <= p.CooldownEndS) {
		add("phases must satisfy warmupEndS <= loadEndS <= cooldownEndS (got %v, %v, %v)", p.WarmupEndS, p.LoadEndS, p.CooldownEndS)
	}
	if p.WarmupEndS < 0 {
		add("phases.warmupEndS must be >= 0")
	}
	st, err1 := time.Parse(time.RFC3339, r.Run.StartedAt)
	en, err2 := time.Parse(time.RFC3339, r.Run.EndedAt)
	if err1 != nil {
		add("run.startedAt is not RFC 3339: %v", err1)
	}
	if err2 != nil {
		add("run.endedAt is not RFC 3339: %v", err2)
	}
	if err1 == nil && err2 == nil && en.Before(st) {
		add("run.endedAt is before run.startedAt")
	}
	s := r.Series
	if !(s.IntervalS > 0) {
		add("series.intervalS must be > 0")
	}
	n := len(s.T)
	for i := 1; i < n; i++ {
		if !(s.T[i] > s.T[i-1]) {
			add("series.t must be strictly increasing (index %d)", i)
			break
		}
	}
	check := func(path string, arr []*float64, allowEmpty bool) {
		if allowEmpty && len(arr) == 0 {
			return
		}
		if len(arr) != n {
			add("series.%s has length %d, expected %d (length of series.t)", path, len(arr), n)
		}
	}
	check("client.p95Ms", s.Client.P95Ms, false)
	check("client.errorRate", s.Client.ErrorRate, false)
	check("client.rps", s.Client.RPS, false)
	switch s.Server.Sampler {
	case SamplerDocker, SamplerPrometheus, SamplerNone:
	default:
		add("series.server.sampler must be docker|prometheus|none (got %q)", s.Server.Sampler)
	}
	noSampler := s.Server.Sampler == SamplerNone
	check("server.rssBytes", s.Server.RSSBytes, noSampler)
	// Optional series are omitted from JSON when empty, so an empty one counts as absent.
	check("server.heapBytes", s.Server.HeapBytes, true)
	check("server.openFds", s.Server.OpenFDs, true)
	check("server.activeSessions", s.Server.ActiveSessions, true)
	if !noSampler && len(s.Server.RSSBytes) == 0 {
		add("series.server.rssBytes is empty but sampler is '%s'", s.Server.Sampler)
	}
	sm := r.Summary
	if sm.Errors > sm.Reqs {
		add("summary.errors > summary.reqs")
	}
	if sm.ErrorRate < 0 || sm.ErrorRate > 1 {
		add("summary.errorRate out of [0,1]")
	}
	var byType int64
	for _, v := range sm.ByErrorType {
		byType += v
	}
	if byType != sm.Errors {
		add("sum of summary.byErrorType (%d) != summary.errors (%d)", byType, sm.Errors)
	}
	names := map[string]bool{}
	for _, t := range r.Tools {
		if names[t.Name] {
			add("duplicate tool '%s'", t.Name)
		}
		names[t.Name] = true
		if t.Errors > t.Reqs {
			add("tools[%s].errors > reqs", t.Name)
		}
		if !(t.P50 <= t.P95 && t.P95 <= t.P99 && t.P99 <= t.Max) {
			add("tools[%s] percentiles not monotonic (p50<=p95<=p99<=max)", t.Name)
		}
	}
	for i, v := range r.Verdicts {
		switch v.Status {
		case StatusPass, StatusFail, StatusWarn, StatusSkipped:
		default:
			add("verdicts[%d].status invalid: %q", i, v.Status)
		}
		if v.R2 != nil && (*v.R2 < 0 || *v.R2 > 1) {
			add("verdicts[%d].r2 out of [0,1]", i)
		}
	}
	return errors.Join(errs...)
}

// Marshal normalizes r and returns its indented JSON encoding (trailing newline).
func Marshal(r *Report) ([]byte, error) {
	r.Normalize()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// WriteJSON normalizes r and writes it as indented JSON to path (via a temp
// file + rename). It creates the parent directory if needed. It does not call Check.
func WriteJSON(path string, r *Report) error {
	b, err := Marshal(r)
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".report-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// ReadJSON reads and decodes a report.json. Unknown properties are ignored.
// It does not call Check.
func ReadJSON(path string) (*Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &r, nil
}

// Passed implements the overall rule: a report fails if any verdict has
// status "fail" or any threshold has passed == false.
func Passed(r *Report) bool {
	for _, v := range r.Verdicts {
		if v.Status == StatusFail {
			return false
		}
	}
	for _, t := range r.Thresholds {
		if !t.Passed {
			return false
		}
	}
	return true
}

// HasWarnings reports whether any verdict has status "warn".
func HasWarnings(r *Report) bool {
	for _, v := range r.Verdicts {
		if v.Status == StatusWarn {
			return true
		}
	}
	return false
}
