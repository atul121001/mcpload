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
	"regexp"
	"sort"
	"strings"
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
	VerdictGenerator       = "generator"
	VerdictToolIsolation   = "tool_isolation"
	VerdictCapacity        = "capacity"
	VerdictSessionSurvival = "session_survival"
	VerdictRecovery        = "recovery"
	VerdictCallIntegrity   = "call_integrity"
)

// VerdictIDs lists every verdict id the schema allows.
var VerdictIDs = []string{
	VerdictMemoryLeak, VerdictSessionLeak, VerdictFDLeak, VerdictLatencyDrift,
	VerdictErrorDrift, VerdictSessionNotFound, VerdictThreshold, VerdictGenerator,
	VerdictToolIsolation, VerdictCapacity, VerdictSessionSurvival, VerdictRecovery,
	VerdictCallIntegrity,
}

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
	// Workflow summarises multi-step agent workflows (scenario agent-workflow); nil otherwise.
	Workflow *Workflow `json:"workflow,omitempty"`
	// Capacity is the per-step result of a step-load run (optional).
	Capacity *Capacity `json:"capacity,omitempty"`
	// Sessions summarises long-lived sessions (scenario long-lived); nil otherwise.
	Sessions *Sessions `json:"sessions,omitempty"`
	// Chaos is the fault mcpload injected during the run (--chaos-restart); nil otherwise.
	Chaos *Chaos `json:"chaos,omitempty"`
	// CallIntegrity compares the calls the client sent with the ones the
	// server executed (--calls-url); nil when not measured.
	CallIntegrity *CallIntegrity `json:"callIntegrity,omitempty"`
}

// Sessions is report.sessions: long-lived sessions and how they ended.
// DiedAfterS is the median lifetime (s) of the sessions that died, nil when
// none did. EarlyP95Ms/LateP95Ms are the p95 of successful calls in the first
// and last third of the sessions for DriftTool, the tool whose p95 rose most
// (nil without enough calls).
type Sessions struct {
	Total        int64            `json:"total"`
	Survived     int64            `json:"survived"`
	Died         int64            `json:"died"`
	DiedByCause  map[string]int64 `json:"diedByCause"`
	DiedAfterS   *float64         `json:"diedAfterS"`
	LifetimeP50S float64          `json:"lifetimeP50S"`
	Reconnects   int64            `json:"reconnects"`
	DriftTool    string           `json:"driftTool,omitempty"`
	EarlyP95Ms   *float64         `json:"earlyP95Ms"`
	LateP95Ms    *float64         `json:"lateP95Ms"`
}

// Chaos is report.chaos: a container restart mcpload ran during the run.
// AtS is when `docker restart` started (seconds since run start), DurationS
// how long the command took. Ran is false when the run ended first.
type Chaos struct {
	Action    string    `json:"action"`
	Container string    `json:"container"`
	Ran       bool      `json:"ran"`
	AtS       float64   `json:"atS"`
	DurationS float64   `json:"durationS"`
	Error     string    `json:"error,omitempty"`
	Recovery  *Recovery `json:"recovery,omitempty"`
}

// Recovery is how the server and its clients came back after the restart
// (verdict recovery). Times are seconds after Chaos.AtS. RecoveryS is the
// start of the first WindowS-long window with requests, an error rate (tool
// errors left out) under ErrorRate and a connect p95 under ConnectP95Ms; nil
// when that never happened. ServerBackS is the first successful connect after
// the first error; LastReconnectS the last reconnect of an agent whose session
// broke. Connect and error counts cover [AtS, AtS+RecoveryS+WindowS] (to the
// end of the run when not recovered).
type Recovery struct {
	Recovered       bool             `json:"recovered"`
	RecoveryS       *float64         `json:"recoveryS"`
	ServerBackS     *float64         `json:"serverBackS"`
	LastReconnectS  *float64         `json:"lastReconnectS"`
	BudgetS         float64          `json:"budgetS"`
	WindowS         float64          `json:"windowS"`
	ErrorRate       float64          `json:"errorRate"`
	ConnectP95Ms    float64          `json:"connectP95Ms"`
	Reconnects      int64            `json:"reconnects"`
	ConnectAttempts int64            `json:"connectAttempts"`
	ConnectFailures int64            `json:"connectFailures"`
	ErrorsByType    map[string]int64 `json:"errorsByType"`
}

// CallIntegrity is report.callIntegrity: per call id (params._meta
// "io.mcpload/callId"), what the client saw against what the server ran.
type CallIntegrity struct {
	// Source is where the server's executions came from (the --calls-url).
	Source string `json:"source"`
	// Tagged call ids sent; Retried: sent more than once; ClientFailed: every attempt failed on the client.
	Tagged       int64 `json:"tagged"`
	Retried      int64 `json:"retried"`
	ClientFailed int64 `json:"clientFailed"`
	// Executed ids the server ran at least once, Executions in all.
	Executed   int64 `json:"executed"`
	Executions int64 `json:"executions"`
	// FailedButExecuted: failed on the client, ran on the server. NeverRan: failed and never ran.
	FailedButExecuted int64 `json:"failedButExecuted"`
	NeverRan          int64 `json:"neverRan"`
	// Duplicated ids ran more than once (DuplicateExecutions extra runs);
	// DuplicatedAfterRetry of them had been sent more than once by the client.
	Duplicated           int64 `json:"duplicated"`
	DuplicateExecutions  int64 `json:"duplicateExecutions"`
	DuplicatedAfterRetry int64 `json:"duplicatedAfterRetry"`
}

// Latency is a set of durations in ms; all zero when Count is 0.
type Latency struct {
	Count int64   `json:"count"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
}

// Workflow is report.workflow: Runs workflows started, Completed of them ran
// every step. DurationMs covers complete workflows (connect to the end of the
// last step, think time included); Steps are in plan order, each the wall time
// of the step's parallel batch.
type Workflow struct {
	Runs           int64          `json:"runs"`
	Completed      int64          `json:"completed"`
	CompletionRate float64        `json:"completionRate"`
	DurationMs     Latency        `json:"durationMs"`
	Steps          []WorkflowStep `json:"steps"`
}

// WorkflowStep is the latency of one plan step.
type WorkflowStep struct {
	Name string `json:"name"`
	Latency
}

// Capacity is the outcome of a step-load run: one entry per concurrency step
// and the breaking point (verdict capacity).
type Capacity struct {
	// PlannedVUs are the steps the scenario planned; Steps may stop short.
	PlannedVUs []int `json:"plannedVus,omitempty"`
	// MinAgents is the MIN_AGENTS target, when set.
	MinAgents *int `json:"minAgents,omitempty"`
	// MaxSustainableVUs is the last step before the first breach (or the
	// highest step when none broke); nil when the first step broke.
	MaxSustainableVUs *int `json:"maxSustainableVus"`
	// BreakingVUs is the first step that crossed a budget; nil when none did.
	BreakingVUs *int `json:"breakingVus"`
	// Inconclusive is set when the load generator was saturated at the
	// breaking step, so the breach may be k6's own overhead.
	Inconclusive bool `json:"inconclusive"`
	// StoppedEarly is set when the scenario aborted the run (ABORT_ERR_RATE).
	StoppedEarly bool            `json:"stoppedEarly"`
	Budgets      CapacityBudgets `json:"budgets"`
	Steps        []Step          `json:"steps"`
}

// CapacityBudgets are the budgets each step was judged against.
type CapacityBudgets struct {
	ConnectP95Ms     float64               `json:"connectP95Ms"`
	ConnectErrorRate float64               `json:"connectErrorRate"`
	Tools            map[string]ToolBudget `json:"tools"`
}

// ToolBudget is one tool's latency (ms) and error-rate budget.
type ToolBudget struct {
	P95Ms     float64 `json:"p95Ms"`
	P99Ms     float64 `json:"p99Ms"`
	ErrorRate float64 `json:"errorRate"`
}

// Step is one concurrency level of a step-load run. StartS/EndS bound its
// tagged requests (seconds since run start). Breached lists the step-level
// fields over budget ("connectP95Ms", "connectErrorRate", "toolCalls");
// Breaches describes every breach in words.
type Step struct {
	VUs                int        `json:"vus"`
	StartS             float64    `json:"startS"`
	EndS               float64    `json:"endS"`
	Reqs               int64      `json:"reqs"`
	Errors             int64      `json:"errors"`
	ErrorRate          float64    `json:"errorRate"`
	RPS                float64    `json:"rps"`
	ConnectP95Ms       *float64   `json:"connectP95Ms"`
	ConnectErrorRate   *float64   `json:"connectErrorRate"`
	GeneratorCPUMaxPct *float64   `json:"generatorCpuMaxPct,omitempty"`
	GeneratorSaturated bool       `json:"generatorSaturated"`
	Passed             bool       `json:"passed"`
	Breached           []string   `json:"breached,omitempty"`
	Breaches           []string   `json:"breaches"`
	Tools              []StepTool `json:"tools"`
}

// StepTool is one tool within a step. P95/P99 cover successful calls (nil
// without one); Breached lists the fields over budget ("p95", "p99", "errorRate").
type StepTool struct {
	Name      string   `json:"name"`
	Reqs      int64    `json:"reqs"`
	Errors    int64    `json:"errors"`
	ErrorRate float64  `json:"errorRate"`
	P95       *float64 `json:"p95"`
	P99       *float64 `json:"p99"`
	Breached  []string `json:"breached,omitempty"`
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
	// Generator describes the load-generator (k6) host; nil when unknown.
	Generator *Generator `json:"generator,omitempty"`
}

// Generator is load-generator resource usage. CPU percentages are the share of
// the machine's total CPU capacity (all cores) used by the k6 process, 0..100;
// nil when not measured.
type Generator struct {
	Cores     int      `json:"cores"`
	CPUAvgPct *float64 `json:"cpuAvgPct"`
	CPUMaxPct *float64 `json:"cpuMaxPct"`
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
	// Iterations is the number of completed k6 iterations (optional).
	Iterations *int64 `json:"iterations,omitempty"`
	// DroppedIterations is k6's dropped_iterations count (optional).
	DroppedIterations *int64 `json:"droppedIterations,omitempty"`
	// ToolErrors is the count of error_type tool_iserror (optional).
	ToolErrors *int64 `json:"toolErrors,omitempty"`
}

// ToolStats are per-tool tools/call statistics. Latencies in ms and computed
// over successful calls only; Errors/ErrorRate count all failed calls.
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
	// Tools maps tool name -> per-tool series (optional, omitted when empty).
	Tools map[string]ToolSeries `json:"tools,omitempty"`
}

// ToolSeries are per-tool client series, parallel to Series.T.
type ToolSeries struct {
	P95Ms []*float64 `json:"p95Ms"`
}

// ClientSeries are k6-side series.
type ClientSeries struct {
	P95Ms     []*float64 `json:"p95Ms"`
	ErrorRate []*float64 `json:"errorRate"`
	RPS       []*float64 `json:"rps"`
	// DroppedIterations per interval (optional, omitted when empty).
	DroppedIterations []*float64 `json:"droppedIterations,omitempty"`
}

// I64 returns a pointer to v.
func I64(v int64) *int64 { return &v }

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
	if r.Workflow != nil && r.Workflow.Steps == nil {
		r.Workflow.Steps = []WorkflowStep{}
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
	all := []*[]*float64{&s.Client.P95Ms, &s.Client.ErrorRate, &s.Client.RPS, &s.Client.DroppedIterations, &s.Server.RSSBytes, &s.Server.HeapBytes, &s.Server.OpenFDs, &s.Server.ActiveSessions}
	for name, ts := range s.Tools {
		if ts.P95Ms == nil {
			ts.P95Ms = []*float64{}
		}
		s.Tools[name] = ts
		all = append(all, &ts.P95Ms)
	}
	if len(s.Tools) == 0 {
		s.Tools = nil
	}
	for _, p := range all {
		for i, v := range *p {
			if v != nil && !finite(*v) {
				(*p)[i] = nil
			}
		}
	}
	if g := r.Run.Generator; g != nil {
		for _, p := range []**float64{&g.CPUAvgPct, &g.CPUMaxPct} {
			if *p != nil && !finite(**p) {
				*p = nil
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
	if ss := r.Sessions; ss != nil {
		if ss.DiedByCause == nil {
			ss.DiedByCause = map[string]int64{}
		}
		for _, p := range []**float64{&ss.DiedAfterS, &ss.EarlyP95Ms, &ss.LateP95Ms} {
			if *p != nil && !finite(**p) {
				*p = nil
			}
		}
	}
	if ch := r.Chaos; ch != nil && ch.Recovery != nil {
		rc := ch.Recovery
		if rc.ErrorsByType == nil {
			rc.ErrorsByType = map[string]int64{}
		}
		for _, p := range []**float64{&rc.RecoveryS, &rc.ServerBackS, &rc.LastReconnectS} {
			if *p != nil && !finite(**p) {
				*p = nil
			}
		}
	}
	if c := r.Capacity; c != nil {
		if c.Steps == nil {
			c.Steps = []Step{}
		}
		if c.Budgets.Tools == nil {
			c.Budgets.Tools = map[string]ToolBudget{}
		}
		for i := range c.Steps {
			st := &c.Steps[i]
			if st.Breaches == nil {
				st.Breaches = []string{}
			}
			if st.Tools == nil {
				st.Tools = []StepTool{}
			}
			for _, p := range []**float64{&st.ConnectP95Ms, &st.ConnectErrorRate, &st.GeneratorCPUMaxPct} {
				if *p != nil && !finite(**p) {
					*p = nil
				}
			}
			for j := range st.Tools {
				for _, p := range []**float64{&st.Tools[j].P95, &st.Tools[j].P99} {
					if *p != nil && !finite(**p) {
						*p = nil
					}
				}
			}
		}
	}
}

// reURI is the "uri" format of ajv-formats (full mode), which report/validate.mjs uses.
var reURI = regexp.MustCompile(`(?i)^(?:[a-z][a-z0-9+\-.]*:)(?:\/?\/(?:(?:[a-z0-9\-._~!$&'()*+,;=:]|%[0-9a-f]{2})*@)?(?:\[(?:(?:(?:(?:[0-9a-f]{1,4}:){6}|::(?:[0-9a-f]{1,4}:){5}|(?:[0-9a-f]{1,4})?::(?:[0-9a-f]{1,4}:){4}|(?:(?:[0-9a-f]{1,4}:){0,1}[0-9a-f]{1,4})?::(?:[0-9a-f]{1,4}:){3}|(?:(?:[0-9a-f]{1,4}:){0,2}[0-9a-f]{1,4})?::(?:[0-9a-f]{1,4}:){2}|(?:(?:[0-9a-f]{1,4}:){0,3}[0-9a-f]{1,4})?::[0-9a-f]{1,4}:|(?:(?:[0-9a-f]{1,4}:){0,4}[0-9a-f]{1,4})?::)(?:[0-9a-f]{1,4}:[0-9a-f]{1,4}|(?:(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\.){3}(?:25[0-5]|2[0-4]\d|[01]?\d\d?))|(?:(?:[0-9a-f]{1,4}:){0,5}[0-9a-f]{1,4})?::[0-9a-f]{1,4}|(?:(?:[0-9a-f]{1,4}:){0,6}[0-9a-f]{1,4})?::)|[Vv][0-9a-f]+\.[a-z0-9\-._~!$&'()*+,;=:]+)\]|(?:(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\.){3}(?:25[0-5]|2[0-4]\d|[01]?\d\d?)|(?:[a-z0-9\-._~!$&'()*+,;=]|%[0-9a-f]{2})*)(?::\d*)?(?:\/(?:[a-z0-9\-._~!$&'()*+,;=:@]|%[0-9a-f]{2})*)*|\/(?:(?:[a-z0-9\-._~!$&'()*+,;=:@]|%[0-9a-f]{2})+(?:\/(?:[a-z0-9\-._~!$&'()*+,;=:@]|%[0-9a-f]{2})*)*)?|(?:[a-z0-9\-._~!$&'()*+,;=:@]|%[0-9a-f]{2})+(?:\/(?:[a-z0-9\-._~!$&'()*+,;=:@]|%[0-9a-f]{2})*)*)(?:\?(?:[a-z0-9\-._~!$&'()*+,;=:@/?]|%[0-9a-f]{2})*)?(?:#(?:[a-z0-9\-._~!$&'()*+,;=:@/?]|%[0-9a-f]{2})*)?$`)

var (
	// reDateTime is the "date-time" format of ajv-formats (RFC 3339 with a time zone).
	reDateTime    = regexp.MustCompile(`(?i)^\d\d\d\d-[0-1]\d-[0-3]\dt(?:[0-2]\d:[0-5]\d:[0-5]\d|23:59:60)(?:\.\d+)?(?:z|[+-]\d\d(?::?\d\d)?)$`)
	reErrorType   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	reGitSHA      = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	knownVerdicts = func() map[string]bool {
		m := map[string]bool{}
		for _, id := range VerdictIDs {
			m[id] = true
		}
		return m
	}()
)

// parseTime parses an RFC 3339 date-time as the JSON schema's "date-time"
// format accepts it (case-insensitive T/Z, offsets with or without a colon).
func parseTime(s string) (time.Time, error) {
	if !reDateTime.MatchString(s) {
		return time.Time{}, fmt.Errorf("%q is not an RFC 3339 date-time", s)
	}
	u := strings.ToUpper(s)
	if n := len(u); n >= 5 && (u[n-5] == '+' || u[n-5] == '-') {
		u = u[:n-2] + ":" + u[n-2:] // +0100 -> +01:00
	} else if n >= 3 && (u[n-3] == '+' || u[n-3] == '-') {
		u += ":00" // +01 -> +01:00
	}
	u = strings.Replace(u, ":60", ":59", 1) // leap second
	return time.Parse(time.RFC3339Nano, u)
}

// Check validates r against report/schema/report.v1.json (required fields,
// enums, patterns, formats, numeric ranges) and the semantic checks of
// report/validate.mjs (parallel array lengths, phase order, byErrorType sum,
// percentile order, ...). It returns all problems joined, or nil.
//
// Go cannot tell an absent string from an empty one, so required strings are
// checked as non-empty (the schema gives them minLength 1; run.target.url must
// match the "uri" format, which rejects ""). Required slices/maps must be
// non-nil; ReadJSON leaves them nil when the property is absent or null.
func (r *Report) Check() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	nonEmpty := func(path, v string) {
		if v == "" {
			add("%s is required and must be non-empty", path)
		}
	}
	nonNeg := func(path string, v float64) {
		if !(v >= 0) || !finite(v) {
			add("%s must be a finite number >= 0 (got %v)", path, v)
		}
	}
	rate := func(path string, v float64) {
		if !(v >= 0 && v <= 1) {
			add("%s must be in [0,1] (got %v)", path, v)
		}
	}

	if r.SchemaVersion != SchemaVersion {
		add("schemaVersion must be %q (got %q)", SchemaVersion, r.SchemaVersion)
	}
	nonEmpty("tool.name", r.Tool.Name)
	nonEmpty("tool.version", r.Tool.Version)

	run := r.Run
	nonEmpty("run.id", run.ID)
	nonEmpty("run.scenario", run.Scenario)
	nonEmpty("run.protocol", run.Protocol)
	nonEmpty("run.k6Version", run.K6Version)
	nonEmpty("run.load.executor", run.Load.Executor)
	nonNeg("run.durationS", run.DurationS)
	if !reURI.MatchString(run.Target.URL) {
		add("run.target.url must be an absolute URI (got %q)", run.Target.URL)
	}
	if run.Git != nil && !reGitSHA.MatchString(run.Git.SHA) {
		add("run.git.sha must match ^[0-9a-f]{7,64}$ (got %q)", run.Git.SHA)
	}
	l := run.Load
	if l.VUs != nil && *l.VUs < 0 {
		add("run.load.vus must be >= 0")
	}
	if l.MaxVUs != nil && *l.MaxVUs < 0 {
		add("run.load.maxVus must be >= 0")
	}
	if l.ArrivalRate != nil {
		nonNeg("run.load.arrivalRate", *l.ArrivalRate)
	}
	if l.ArrivalTimeUnitS != nil && !(*l.ArrivalTimeUnitS > 0) {
		add("run.load.arrivalTimeUnitS must be > 0")
	}
	if g := run.Generator; g != nil {
		if g.Cores < 1 {
			add("run.generator.cores must be >= 1 (got %d)", g.Cores)
		}
		if p := g.CPUAvgPct; p != nil && !(*p >= 0 && *p <= 100) {
			add("run.generator.cpuAvgPct must be in [0,100] or null (got %v)", *p)
		}
		if p := g.CPUMaxPct; p != nil && !(*p >= 0 && *p <= 100) {
			add("run.generator.cpuMaxPct must be in [0,100] or null (got %v)", *p)
		}
	}
	st, err1 := parseTime(run.StartedAt)
	en, err2 := parseTime(run.EndedAt)
	if err1 != nil {
		add("run.startedAt: %v", err1)
	}
	if err2 != nil {
		add("run.endedAt: %v", err2)
	}
	if err1 == nil && err2 == nil && en.Before(st) {
		add("run.endedAt is before run.startedAt")
	}

	p := r.Phases
	nonNeg("phases.warmupEndS", p.WarmupEndS)
	nonNeg("phases.loadEndS", p.LoadEndS)
	nonNeg("phases.cooldownEndS", p.CooldownEndS)
	if !(p.WarmupEndS <= p.LoadEndS && p.LoadEndS <= p.CooldownEndS) {
		add("phases must satisfy warmupEndS <= loadEndS <= cooldownEndS (got %v, %v, %v)", p.WarmupEndS, p.LoadEndS, p.CooldownEndS)
	}

	sm := r.Summary
	if sm.Reqs < 0 || sm.Errors < 0 {
		add("summary.reqs and summary.errors must be >= 0")
	}
	if sm.Errors > sm.Reqs {
		add("summary.errors > summary.reqs")
	}
	rate("summary.errorRate", sm.ErrorRate)
	if sm.ByErrorType == nil {
		add("summary.byErrorType is required")
	}
	var byType int64
	for k, v := range sm.ByErrorType {
		if !reErrorType.MatchString(k) {
			add("summary.byErrorType key %q must match ^[a-z][a-z0-9_]*$", k)
		}
		if v < 0 {
			add("summary.byErrorType[%s] must be >= 0", k)
		}
		byType += v
	}
	if byType != sm.Errors {
		add("sum of summary.byErrorType (%d) != summary.errors (%d)", byType, sm.Errors)
	}
	if sm.Iterations != nil && *sm.Iterations < 0 {
		add("summary.iterations must be >= 0")
	}
	if sm.DroppedIterations != nil && *sm.DroppedIterations < 0 {
		add("summary.droppedIterations must be >= 0")
	}
	if sm.ToolErrors != nil && *sm.ToolErrors < 0 {
		add("summary.toolErrors must be >= 0")
	}

	if r.Tools == nil {
		add("tools is required")
	}
	names := map[string]bool{}
	for _, t := range r.Tools {
		nonEmpty("tools[].name", t.Name)
		if names[t.Name] {
			add("duplicate tool '%s'", t.Name)
		}
		names[t.Name] = true
		if t.Reqs < 0 || t.Errors < 0 {
			add("tools[%s].reqs and errors must be >= 0", t.Name)
		}
		if t.Errors > t.Reqs {
			add("tools[%s].errors > reqs", t.Name)
		}
		rate(fmt.Sprintf("tools[%s].errorRate", t.Name), t.ErrorRate)
		nonNeg(fmt.Sprintf("tools[%s].p50", t.Name), t.P50)
		nonNeg(fmt.Sprintf("tools[%s].p95", t.Name), t.P95)
		nonNeg(fmt.Sprintf("tools[%s].p99", t.Name), t.P99)
		nonNeg(fmt.Sprintf("tools[%s].max", t.Name), t.Max)
		if !(t.P50 <= t.P95 && t.P95 <= t.P99 && t.P99 <= t.Max) {
			add("tools[%s] percentiles not monotonic (p50<=p95<=p99<=max)", t.Name)
		}
	}

	if w := r.Workflow; w != nil {
		if w.Runs < 0 || w.Completed < 0 {
			add("workflow.runs and workflow.completed must be >= 0")
		}
		if w.Completed > w.Runs {
			add("workflow.completed > workflow.runs")
		}
		rate("workflow.completionRate", w.CompletionRate)
		latency := func(path string, l Latency) {
			if l.Count < 0 {
				add("%s.count must be >= 0", path)
			}
			nonNeg(path+".p50", l.P50)
			nonNeg(path+".p95", l.P95)
			nonNeg(path+".p99", l.P99)
			nonNeg(path+".max", l.Max)
			if !(l.P50 <= l.P95 && l.P95 <= l.P99 && l.P99 <= l.Max) {
				add("%s percentiles not monotonic (p50<=p95<=p99<=max)", path)
			}
		}
		latency("workflow.durationMs", w.DurationMs)
		if w.Steps == nil {
			add("workflow.steps is required")
		}
		steps := map[string]bool{}
		for _, st := range w.Steps {
			nonEmpty("workflow.steps[].name", st.Name)
			if steps[st.Name] {
				add("duplicate workflow step '%s'", st.Name)
			}
			steps[st.Name] = true
			latency(fmt.Sprintf("workflow.steps[%s]", st.Name), st.Latency)
		}
	}

	if r.Thresholds == nil {
		add("thresholds is required")
	}
	for i, t := range r.Thresholds {
		nonEmpty(fmt.Sprintf("thresholds[%d].metric", i), t.Metric)
		nonEmpty(fmt.Sprintf("thresholds[%d].expr", i), t.Expr)
	}

	s := r.Series
	if !(s.IntervalS > 0) {
		add("series.intervalS must be > 0")
	}
	if s.T == nil {
		add("series.t is required")
	}
	n := len(s.T)
	for i, ti := range s.T {
		if !(ti >= 0) {
			add("series.t[%d] must be >= 0", i)
			break
		}
	}
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
	// Optional series are omitted from JSON when empty, so an empty one counts as absent.
	check("client.droppedIterations", s.Client.DroppedIterations, true)
	switch s.Server.Sampler {
	case SamplerDocker, SamplerPrometheus, SamplerNone:
	default:
		add("series.server.sampler must be docker|prometheus|none (got %q)", s.Server.Sampler)
	}
	noSampler := s.Server.Sampler == SamplerNone
	if s.Server.RSSBytes == nil {
		add("series.server.rssBytes is required")
	}
	check("server.rssBytes", s.Server.RSSBytes, noSampler)
	check("server.heapBytes", s.Server.HeapBytes, true)
	check("server.openFds", s.Server.OpenFDs, true)
	check("server.activeSessions", s.Server.ActiveSessions, true)
	if !noSampler && len(s.Server.RSSBytes) == 0 {
		add("series.server.rssBytes is empty but sampler is '%s'", s.Server.Sampler)
	}
	tools := make([]string, 0, len(s.Tools))
	for name := range s.Tools {
		tools = append(tools, name)
	}
	sort.Strings(tools)
	for _, name := range tools {
		if s.Tools[name].P95Ms == nil {
			add("series.tools[%s].p95Ms is required", name)
			continue
		}
		check(fmt.Sprintf("tools[%s].p95Ms", name), s.Tools[name].P95Ms, false)
	}

	if r.Verdicts == nil {
		add("verdicts is required")
	}
	for i, v := range r.Verdicts {
		if !knownVerdicts[v.ID] {
			add("verdicts[%d].id invalid: %q", i, v.ID)
		}
		switch v.Status {
		case StatusPass, StatusFail, StatusWarn, StatusSkipped:
		default:
			add("verdicts[%d].status invalid: %q", i, v.Status)
		}
		if v.R2 != nil && !(*v.R2 >= 0 && *v.R2 <= 1) {
			add("verdicts[%d].r2 out of [0,1]", i)
		}
	}

	if ss := r.Sessions; ss != nil {
		if ss.Total < 0 || ss.Survived < 0 || ss.Died < 0 || ss.Reconnects < 0 {
			add("sessions counts must be >= 0")
		}
		if ss.Survived+ss.Died != ss.Total {
			add("sessions.survived + sessions.died (%d) != sessions.total (%d)", ss.Survived+ss.Died, ss.Total)
		}
		if ss.DiedByCause == nil {
			add("sessions.diedByCause is required")
		}
		var byCause int64
		for k, v := range ss.DiedByCause {
			if !reErrorType.MatchString(k) || v < 0 {
				add("sessions.diedByCause[%s] invalid", k)
			}
			byCause += v
		}
		if byCause != ss.Died {
			add("sum of sessions.diedByCause (%d) != sessions.died (%d)", byCause, ss.Died)
		}
		nonNeg("sessions.lifetimeP50S", ss.LifetimeP50S)
		for _, p := range []struct {
			path string
			v    *float64
		}{{"sessions.diedAfterS", ss.DiedAfterS}, {"sessions.earlyP95Ms", ss.EarlyP95Ms}, {"sessions.lateP95Ms", ss.LateP95Ms}} {
			if p.v != nil {
				nonNeg(p.path, *p.v)
			}
		}
	}
	if ch := r.Chaos; ch != nil {
		if ch.Action != "restart" {
			add("chaos.action must be \"restart\" (got %q)", ch.Action)
		}
		nonEmpty("chaos.container", ch.Container)
		nonNeg("chaos.atS", ch.AtS)
		nonNeg("chaos.durationS", ch.DurationS)
		if rc := ch.Recovery; rc != nil {
			if rc.Recovered != (rc.RecoveryS != nil) {
				add("chaos.recovery.recoveryS must be set exactly when recovered is true")
			}
			for _, p := range []struct {
				path string
				v    *float64
			}{{"chaos.recovery.recoveryS", rc.RecoveryS}, {"chaos.recovery.serverBackS", rc.ServerBackS}, {"chaos.recovery.lastReconnectS", rc.LastReconnectS}} {
				if p.v != nil {
					nonNeg(p.path, *p.v)
				}
			}
			nonNeg("chaos.recovery.budgetS", rc.BudgetS)
			nonNeg("chaos.recovery.windowS", rc.WindowS)
			rate("chaos.recovery.errorRate", rc.ErrorRate)
			nonNeg("chaos.recovery.connectP95Ms", rc.ConnectP95Ms)
			if rc.Reconnects < 0 || rc.ConnectAttempts < 0 || rc.ConnectFailures < 0 || rc.ConnectFailures > rc.ConnectAttempts {
				add("chaos.recovery: counts must be >= 0 and connectFailures <= connectAttempts")
			}
			if rc.ErrorsByType == nil {
				add("chaos.recovery.errorsByType is required")
			}
			for k, v := range rc.ErrorsByType {
				if !reErrorType.MatchString(k) || v < 0 {
					add("chaos.recovery.errorsByType[%s] invalid", k)
				}
			}
		}
	}
	if ci := r.CallIntegrity; ci != nil {
		nonEmpty("callIntegrity.source", ci.Source)
		for _, p := range []struct {
			path string
			v    int64
		}{{"tagged", ci.Tagged}, {"retried", ci.Retried}, {"clientFailed", ci.ClientFailed}, {"executed", ci.Executed}, {"executions", ci.Executions},
			{"failedButExecuted", ci.FailedButExecuted}, {"neverRan", ci.NeverRan}, {"duplicated", ci.Duplicated}, {"duplicateExecutions", ci.DuplicateExecutions}, {"duplicatedAfterRetry", ci.DuplicatedAfterRetry}} {
			if p.v < 0 {
				add("callIntegrity.%s must be >= 0", p.path)
			}
		}
		if ci.FailedButExecuted+ci.NeverRan != ci.ClientFailed {
			add("callIntegrity.failedButExecuted + neverRan (%d) != clientFailed (%d)", ci.FailedButExecuted+ci.NeverRan, ci.ClientFailed)
		}
		if ci.Executed > ci.Executions || ci.Duplicated > ci.Executed || ci.DuplicatedAfterRetry > ci.Duplicated {
			add("callIntegrity: executed <= executions, duplicated <= executed and duplicatedAfterRetry <= duplicated must hold")
		}
	}
	if c := r.Capacity; c != nil {
		if c.Steps == nil {
			add("capacity.steps is required")
		}
		for _, p := range []struct {
			path string
			v    *int
		}{{"capacity.minAgents", c.MinAgents}, {"capacity.maxSustainableVus", c.MaxSustainableVUs}, {"capacity.breakingVus", c.BreakingVUs}} {
			if p.v != nil && *p.v < 1 {
				add("%s must be >= 1", p.path)
			}
		}
		for i, v := range c.PlannedVUs {
			if v < 1 || (i > 0 && v <= c.PlannedVUs[i-1]) {
				add("capacity.plannedVus must be increasing integers >= 1")
				break
			}
		}
		nonNeg("capacity.budgets.connectP95Ms", c.Budgets.ConnectP95Ms)
		rate("capacity.budgets.connectErrorRate", c.Budgets.ConnectErrorRate)
		for name, b := range c.Budgets.Tools {
			nonNeg(fmt.Sprintf("capacity.budgets.tools[%s].p95Ms", name), b.P95Ms)
			nonNeg(fmt.Sprintf("capacity.budgets.tools[%s].p99Ms", name), b.P99Ms)
			rate(fmt.Sprintf("capacity.budgets.tools[%s].errorRate", name), b.ErrorRate)
		}
		for i, st := range c.Steps {
			path := fmt.Sprintf("capacity.steps[%d]", i)
			if st.VUs < 1 {
				add("%s.vus must be >= 1", path)
			}
			if i > 0 && st.VUs <= c.Steps[i-1].VUs {
				add("capacity.steps must be sorted by strictly increasing vus (index %d)", i)
			}
			nonNeg(path+".startS", st.StartS)
			nonNeg(path+".endS", st.EndS)
			if st.EndS < st.StartS {
				add("%s.endS is before startS", path)
			}
			if st.Reqs < 0 || st.Errors < 0 || st.Errors > st.Reqs {
				add("%s: errors must be in [0, reqs]", path)
			}
			rate(path+".errorRate", st.ErrorRate)
			nonNeg(path+".rps", st.RPS)
			if st.ConnectP95Ms != nil {
				nonNeg(path+".connectP95Ms", *st.ConnectP95Ms)
			}
			if st.ConnectErrorRate != nil {
				rate(path+".connectErrorRate", *st.ConnectErrorRate)
			}
			if p := st.GeneratorCPUMaxPct; p != nil && !(*p >= 0 && *p <= 100) {
				add("%s.generatorCpuMaxPct must be in [0,100]", path)
			}
			if st.Breaches == nil || st.Tools == nil {
				add("%s.breaches and .tools are required", path)
			}
			for _, b := range st.Breached {
				if b != "connectP95Ms" && b != "connectErrorRate" && b != "toolCalls" {
					add("%s.breached has an invalid entry %q", path, b)
				}
			}
			for _, t := range st.Tools {
				tp := fmt.Sprintf("%s.tools[%s]", path, t.Name)
				nonEmpty(path+".tools[].name", t.Name)
				if t.Reqs < 0 || t.Errors < 0 || t.Errors > t.Reqs {
					add("%s: errors must be in [0, reqs]", tp)
				}
				rate(tp+".errorRate", t.ErrorRate)
				if t.P95 != nil {
					nonNeg(tp+".p95", *t.P95)
				}
				if t.P99 != nil {
					nonNeg(tp+".p99", *t.P99)
				}
				for _, b := range t.Breached {
					if b != "p95" && b != "p99" && b != "errorRate" {
						add("%s.breached has an invalid entry %q", tp, b)
					}
				}
				if t.P95 != nil && t.P99 != nil && *t.P95 > *t.P99 {
					add("%s percentiles not monotonic (p95<=p99)", tp)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// Marshal normalizes r and returns its indented JSON encoding (trailing newline).
func Marshal(r *Report) ([]byte, error) {
	r.Normalize()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
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
