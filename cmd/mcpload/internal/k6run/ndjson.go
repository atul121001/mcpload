package k6run

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

// Metric names emitted by xk6-mcpload.
const (
	MetricReqDuration     = "mcp_req_duration"
	MetricReqs            = "mcp_reqs"
	MetricErrors          = "mcp_errors"
	MetricConnectDuration = "mcp_connect_duration"
	methodToolsCall       = "tools/call"

	// Custom metrics of scenarios/agent-workflow.js.
	MetricWorkflowDuration     = "mcp_workflow_duration"
	MetricWorkflowStepDuration = "mcp_workflow_step_duration"
	MetricWorkflowComplete     = "mcp_workflow_complete"

	// k6 built-in metrics.
	MetricIterations        = "iterations"
	MetricDroppedIterations = "dropped_iterations"

	// ErrorTypeToolIsError is the error_type of a tools/call whose result had isError: true.
	ErrorTypeToolIsError = "tool_iserror"
)

type line struct {
	Type   string `json:"type"`
	Metric string `json:"metric"`
	Data   struct {
		// Point
		Time  string            `json:"time"`
		Value float64           `json:"value"`
		Tags  map[string]string `json:"tags"`
		// Metric
		Name string `json:"name"`
		Kind string `json:"type"`
	} `json:"data"`
}

// forEachLine streams the NDJSON file without loading it whole.
func forEachLine(r io.Reader, fn func(l *line) error) error {
	br := bufio.NewReaderSize(r, 1<<20)
	var l line
	for {
		b, err := br.ReadBytes('\n')
		b = bytes.TrimSpace(b)
		if len(b) > 0 {
			l = line{}
			if jerr := json.Unmarshal(b, &l); jerr != nil {
				// A truncated final line (k6 killed) is tolerated.
				if err == io.EOF {
					return nil
				}
				return fmt.Errorf("k6 NDJSON: %w", jerr)
			}
			if ferr := fn(&l); ferr != nil {
				return ferr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// TimeRange returns the earliest and latest sample time in an NDJSON stream.
func TimeRange(r io.Reader) (first, last time.Time, err error) {
	err = forEachLine(r, func(l *line) error {
		if l.Type != "Point" {
			return nil
		}
		t, perr := time.Parse(time.RFC3339Nano, l.Data.Time)
		if perr != nil {
			return nil
		}
		if first.IsZero() || t.Before(first) {
			first = t
		}
		if t.After(last) {
			last = t
		}
		return nil
	})
	return first, last, err
}

type bucket struct {
	reqs      float64
	errors    float64
	dropped   float64
	durations []float64
	// tools holds successful tools/call durations per tool.
	tools map[string][]float64
}

// merge folds o into b.
func (b *bucket) merge(o bucket) {
	b.reqs += o.reqs
	b.errors += o.errors
	b.dropped += o.dropped
	b.durations = append(b.durations, o.durations...)
	for n, d := range o.tools {
		if b.tools == nil {
			b.tools = map[string][]float64{}
		}
		b.tools[n] = append(b.tools[n], d...)
	}
}

type toolAgg struct {
	reqs, errors float64
	samples      int       // all duration samples (successful or not)
	durations    []float64 // successful calls only
}

// Aggregator folds NDJSON points into report aggregates.
type Aggregator struct {
	Origin   time.Time
	Interval time.Duration

	buckets     []bucket
	tools       map[string]*toolAgg
	reqs        float64
	errors      float64
	iterations  float64
	dropped     float64
	byErrorType map[string]float64
	protoOK     map[string]int
	protoAny    map[string]int
	metricTypes map[string]string
	tracked     []*series
	// scenarioTools holds successful tools/call durations per k6 scenario
	// (the 'scenario' system tag) and tool, for phase comparisons.
	scenarioTools map[string]map[string][]float64
	wf            workflowAgg
	first, last   time.Time
	points        int
}

// NewAggregator creates an aggregator whose buckets start at origin. Every
// threshold key in track gets its samples collected for observed values.
func NewAggregator(origin time.Time, interval time.Duration, track []string) *Aggregator {
	a := &Aggregator{
		Origin:      origin,
		Interval:    interval,
		tools:       map[string]*toolAgg{},
		byErrorType: map[string]float64{},
		protoOK:     map[string]int{},
		protoAny:    map[string]int{},
		metricTypes: map[string]string{},

		scenarioTools: map[string]map[string][]float64{},
	}
	seen := map[string]bool{}
	for _, k := range track {
		if seen[k] {
			continue
		}
		seen[k] = true
		if sel, err := ParseSelector(k); err == nil {
			a.tracked = append(a.tracked, &series{sel: sel})
		}
	}
	return a
}

func (a *Aggregator) bucketFor(t time.Time) *bucket {
	i := 0
	if d := t.Sub(a.Origin); d > 0 {
		i = int(d / a.Interval)
	}
	for len(a.buckets) <= i {
		a.buckets = append(a.buckets, bucket{})
	}
	return &a.buckets[i]
}

func (a *Aggregator) tool(name string) *toolAgg {
	t := a.tools[name]
	if t == nil {
		t = &toolAgg{}
		a.tools[name] = t
	}
	return t
}

// Read consumes an NDJSON stream.
func (a *Aggregator) Read(r io.Reader) error {
	return forEachLine(r, a.add)
}

func (a *Aggregator) add(l *line) error {
	switch l.Type {
	case "Metric":
		name := l.Data.Name
		if name == "" {
			name = l.Metric
		}
		a.metricTypes[name] = l.Data.Kind
		return nil
	case "Point":
	default:
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, l.Data.Time)
	if err != nil {
		return nil
	}
	a.points++
	if a.first.IsZero() || t.Before(a.first) {
		a.first = t
	}
	if t.After(a.last) {
		a.last = t
	}
	v, tags := l.Data.Value, l.Data.Tags
	for _, s := range a.tracked {
		if s.sel.Matches(l.Metric, tags) {
			s.values = append(s.values, v)
		}
	}
	switch l.Metric {
	case MetricReqDuration:
		b := a.bucketFor(t)
		b.durations = append(b.durations, v)
		if name := tags["tool"]; tags["method"] == methodToolsCall && name != "" {
			ta := a.tool(name)
			ta.samples++
			// xk6-mcpload sets error_type only on failed requests; tool
			// latency percentiles and series cover successful calls only.
			if tags["error_type"] == "" {
				ta.durations = append(ta.durations, v)
				if b.tools == nil {
					b.tools = map[string][]float64{}
				}
				b.tools[name] = append(b.tools[name], v)
				if sc := tags["scenario"]; sc != "" {
					m := a.scenarioTools[sc]
					if m == nil {
						m = map[string][]float64{}
						a.scenarioTools[sc] = m
					}
					m[name] = append(m[name], v)
				}
			}
		}
	case MetricIterations:
		a.iterations += v
	case MetricDroppedIterations:
		a.dropped += v
		a.bucketFor(t).dropped += v
	case MetricReqs:
		a.reqs += v
		a.bucketFor(t).reqs += v
		if tags["method"] == methodToolsCall && tags["tool"] != "" {
			a.tool(tags["tool"]).reqs += v
		}
		if p := tags["protocol"]; p != "" {
			a.protoAny[p]++
		}
	case MetricErrors:
		a.errors += v
		a.bucketFor(t).errors += v
		et := tags["error_type"]
		if et == "" {
			et = "unknown"
		}
		a.byErrorType[et] += v
		if tags["method"] == methodToolsCall && tags["tool"] != "" {
			a.tool(tags["tool"]).errors += v
		}
	case MetricWorkflowDuration:
		a.wf.durations = append(a.wf.durations, v)
	case MetricWorkflowStepDuration:
		a.wf.addStep(tags["step"], v)
	case MetricWorkflowComplete:
		a.wf.runs++
		if v != 0 {
			a.wf.completed++
		}
	case MetricConnectDuration:
		if p := tags["protocol"]; p != "" && tags["error_type"] == "" && strings.HasPrefix(tags["status"], "2") {
			a.protoOK[p]++
		}
	}
	return nil
}

// Points is the number of samples read.
func (a *Aggregator) Points() int { return a.points }

// First and Last are the earliest and latest sample times seen.
func (a *Aggregator) First() time.Time { return a.first }
func (a *Aggregator) Last() time.Time  { return a.last }

// Summary totals (report.summary).
type Summary struct {
	Reqs        int64
	Errors      int64
	ErrorRate   float64
	ByErrorType map[string]int64
	// Iterations and DroppedIterations are k6's iterations and
	// dropped_iterations counters; ToolErrors is mcp_errors{error_type:tool_iserror}.
	Iterations, DroppedIterations, ToolErrors int64
}

// Summary returns the totals over the whole run.
func (a *Aggregator) Summary() Summary {
	s := Summary{Reqs: round(a.reqs), Errors: round(a.errors), ByErrorType: map[string]int64{}}
	for k, v := range a.byErrorType {
		s.ByErrorType[sanitizeKey(k)] += round(v)
	}
	s.ErrorRate = ratio(s.Errors, s.Reqs)
	s.Iterations = round(a.iterations)
	s.DroppedIterations = round(a.dropped)
	s.ToolErrors = round(a.byErrorType[ErrorTypeToolIsError])
	return s
}

// ToolStats is report.tools[i]; latencies in ms.
type ToolStats struct {
	Name               string
	Reqs, Errors       int64
	ErrorRate          float64
	P50, P95, P99, Max float64
}

// Tools returns per-tool stats sorted by name.
func (a *Aggregator) Tools() []ToolStats {
	names := make([]string, 0, len(a.tools))
	for n := range a.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]ToolStats, 0, len(names))
	for _, n := range names {
		t := a.tools[n]
		sort.Float64s(t.durations)
		ts := ToolStats{Name: n, Reqs: round(t.reqs), Errors: round(t.errors)}
		if ts.Reqs < int64(t.samples) {
			ts.Reqs = int64(t.samples)
		}
		if ts.Errors > ts.Reqs {
			ts.Errors = ts.Reqs
		}
		ts.ErrorRate = ratio(ts.Errors, ts.Reqs)
		if len(t.durations) > 0 {
			ts.P50 = Percentile(t.durations, 0.50)
			ts.P95 = Percentile(t.durations, 0.95)
			ts.P99 = Percentile(t.durations, 0.99)
			ts.Max = t.durations[len(t.durations)-1]
		}
		out = append(out, ts)
	}
	return out
}

// PhaseTool is the latency of one tool's successful calls within one k6
// scenario; latencies in ms.
type PhaseTool struct {
	Calls    int
	P50, P95 float64
}

// ScenarioTools returns per-tool latency of successful tools/call within the
// k6 scenario of that name (nil when the scenario produced no tool calls).
func (a *Aggregator) ScenarioTools(scenario string) map[string]PhaseTool {
	m := a.scenarioTools[scenario]
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

// workflowAgg collects the agent-workflow scenario's custom metrics.
type workflowAgg struct {
	runs, completed float64
	durations       []float64
	stepOrder       []string // first-seen order, which is plan order
	steps           map[string][]float64
}

func (w *workflowAgg) addStep(name string, v float64) {
	if name == "" {
		name = "unnamed"
	}
	if w.steps == nil {
		w.steps = map[string][]float64{}
	}
	if _, ok := w.steps[name]; !ok {
		w.stepOrder = append(w.stepOrder, name)
	}
	w.steps[name] = append(w.steps[name], v)
}

// Latency summarises a set of durations (ms); all zero when Count is 0.
type Latency struct {
	Count              int64
	P50, P95, P99, Max float64
}

func latencyOf(d []float64) Latency {
	if len(d) == 0 {
		return Latency{}
	}
	s := append([]float64(nil), d...)
	sort.Float64s(s)
	return Latency{Count: int64(len(s)), P50: Percentile(s, 0.50), P95: Percentile(s, 0.95), P99: Percentile(s, 0.99), Max: s[len(s)-1]}
}

// WorkflowStep is the wall time of one plan step (its slowest parallel call).
type WorkflowStep struct {
	Name string
	Latency
}

// WorkflowStats is report.workflow: Runs and Completed count
// mcp_workflow_complete samples, Duration is mcp_workflow_duration (complete
// workflows) and Steps is mcp_workflow_step_duration by its step tag.
type WorkflowStats struct {
	Runs, Completed int64
	Duration        Latency
	Steps           []WorkflowStep
}

// Workflow returns the workflow metrics, or nil when the run emitted none
// (any scenario other than agent-workflow).
func (a *Aggregator) Workflow() *WorkflowStats {
	w := a.wf
	if w.runs == 0 && len(w.durations) == 0 && len(w.steps) == 0 {
		return nil
	}
	ws := &WorkflowStats{Runs: round(w.runs), Completed: round(w.completed), Duration: latencyOf(w.durations)}
	for _, n := range w.stepOrder {
		ws.Steps = append(ws.Steps, WorkflowStep{Name: n, Latency: latencyOf(w.steps[n])})
	}
	return ws
}

// ClientSeries is report.series.client; each slice has n entries.
// DroppedIterations counts k6 dropped_iterations per bucket (0 when none).
// Tools maps each tool to the p95 (ms) of its successful tools/call durations
// per bucket (nil for buckets without successful calls).
type ClientSeries struct {
	P95Ms, ErrorRate, RPS, DroppedIterations []*float64
	Tools                                    map[string][]*float64
}

// Buckets is the number of client buckets that saw data.
func (a *Aggregator) Buckets() int { return len(a.buckets) }

// Client returns n buckets of client series; samples past the last bucket
// (clock skew at shutdown) are folded into it. See Series.
func (a *Aggregator) Client(n int) ClientSeries { return a.Series(n, true) }

// Series returns n buckets of client series. p95 and error rate are null for
// buckets without requests; rps and dropped iterations are 0 there. With
// foldTail, samples past bucket n-1 are folded into it; otherwise they are
// left out of the series (a trimmed partial last bucket) while still counting
// in Summary and Tools.
func (a *Aggregator) Series(n int, foldTail bool) ClientSeries {
	cs := ClientSeries{
		P95Ms:             make([]*float64, n),
		ErrorRate:         make([]*float64, n),
		RPS:               make([]*float64, n),
		DroppedIterations: make([]*float64, n),
		Tools:             map[string][]*float64{},
	}
	for name := range a.tools {
		cs.Tools[name] = make([]*float64, n)
	}
	iv := a.Interval.Seconds()
	for i := 0; i < n; i++ {
		var b bucket
		switch {
		case i == n-1 && foldTail && len(a.buckets) > n:
			for _, o := range a.buckets[i:] {
				b.merge(o)
			}
		case i < len(a.buckets):
			b = a.buckets[i]
		}
		if len(b.durations) > 0 {
			sort.Float64s(b.durations)
			cs.P95Ms[i] = ptr(Percentile(b.durations, 0.95))
		}
		if b.reqs > 0 {
			cs.ErrorRate[i] = ptr(math.Min(1, b.errors/b.reqs))
		}
		cs.RPS[i] = ptr(b.reqs / iv)
		cs.DroppedIterations[i] = ptr(b.dropped)
		for name, d := range b.tools {
			if len(d) == 0 {
				continue
			}
			sort.Float64s(d)
			cs.Tools[name][i] = ptr(Percentile(d, 0.95))
		}
	}
	return cs
}

// KeepBuckets returns how many interval buckets a run of durationS seconds
// gets: ceil(durationS/interval), minus a trailing bucket that covers less
// than half an interval (its rate would show a false drop and skew fits).
// trimmed reports whether that bucket was dropped. At least one bucket is kept.
func KeepBuckets(durationS float64, interval time.Duration) (n int, trimmed bool) {
	iv := interval.Seconds()
	n = int(math.Ceil(durationS/iv - 1e-9))
	if n < 1 {
		return 1, false
	}
	if n > 1 && durationS-float64(n-1)*iv < 0.5*iv {
		return n - 1, true
	}
	return n, false
}

// Protocol returns the negotiated protocol: the most frequent protocol tag on
// successful connects, else on any request. "" when unknown.
func (a *Aggregator) Protocol() string {
	if p := mostFrequent(a.protoOK); p != "" {
		return p
	}
	return mostFrequent(a.protoAny)
}

func mostFrequent(m map[string]int) string {
	best, bn := "", 0
	for k, n := range m {
		if n > bn || (n == bn && k < best) {
			best, bn = k, n
		}
	}
	return best
}

// Threshold is report.thresholds[i].
type Threshold struct {
	Metric   string
	Expr     string
	Passed   bool
	Observed *float64
}

// Thresholds merges the script's thresholds (from k6 inspect) with k6's own
// pass/fail verdicts (summary export). observed is computed from the NDJSON
// samples. Entries the summary export has but inspect missed are included.
func (a *Aggregator) Thresholds(defs []ThresholdDef, sum *SummaryExport, durationS float64) []Threshold {
	type key struct{ m, e string }
	seen := map[key]bool{}
	all := append([]ThresholdDef(nil), defs...)
	for _, d := range defs {
		seen[key{d.Metric, d.Expr}] = true
	}
	if sum != nil {
		var extra []ThresholdDef
		for m, sm := range sum.Metrics {
			for e := range sm.Thresholds {
				if !seen[key{m, e}] {
					seen[key{m, e}] = true
					extra = append(extra, ThresholdDef{Metric: m, Expr: e})
				}
			}
		}
		sort.Slice(extra, func(i, j int) bool {
			if extra[i].Metric != extra[j].Metric {
				return extra[i].Metric < extra[j].Metric
			}
			return extra[i].Expr < extra[j].Expr
		})
		all = append(all, extra...)
	}
	out := make([]Threshold, 0, len(all))
	for _, d := range all {
		th := Threshold{Metric: d.Metric, Expr: d.Expr, Passed: true}
		expr, exprErr := ParseExpr(d.Expr)
		if s := a.trackedFor(d.Metric); s != nil && exprErr == nil {
			if v, ok := s.aggregate(a.metricTypes[s.sel.Metric], expr.Agg, durationS); ok && !math.IsNaN(v) && !math.IsInf(v, 0) {
				th.Observed = ptr(v)
			}
		}
		decided := false
		if sum != nil {
			if sm, ok := sum.Metrics[d.Metric]; ok {
				if failed, ok := sm.Thresholds[d.Expr]; ok {
					th.Passed, decided = !failed, true
				}
			}
		}
		if !decided && th.Observed != nil && exprErr == nil {
			th.Passed = expr.Holds(*th.Observed)
		}
		out = append(out, th)
	}
	return out
}

func (a *Aggregator) trackedFor(key string) *series {
	for _, s := range a.tracked {
		if s.sel.Key == key {
			return s
		}
	}
	return nil
}

// ParseFile runs both passes over an NDJSON file: the first finds the earliest
// sample (the run origin), the second aggregates relative to it.
func ParseFile(path string, interval time.Duration, track []string) (*Aggregator, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	first, _, err := TimeRange(f)
	if err != nil {
		return nil, err
	}
	if first.IsZero() {
		return nil, errors.New("k6 produced no metric samples")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	a := NewAggregator(first, interval, track)
	if err := a.Read(f); err != nil {
		return nil, err
	}
	return a, nil
}

// ServerPoint is one sampler reading (nil = not available).
type ServerPoint struct {
	T                                            time.Time
	RSSBytes, HeapBytes, OpenFDs, ActiveSessions *float64
}

// ServerSeries is report.series.server (without the sampler name).
type ServerSeries struct {
	RSSBytes, HeapBytes, OpenFDs, ActiveSessions []*float64
}

// AlignServer averages sampler points into n buckets of interval starting at
// origin. Points before origin go to bucket 0 only if within one interval;
// points after the last bucket go to the last one (see AlignServerN).
func AlignServer(points []ServerPoint, origin time.Time, interval time.Duration, n int) ServerSeries {
	return AlignServerN(points, origin, interval, n, true)
}

// AlignServerN is AlignServer; without foldTail, points at or after the end
// of bucket n-1 are dropped (used when a partial last bucket was trimmed).
func AlignServerN(points []ServerPoint, origin time.Time, interval time.Duration, n int, foldTail bool) ServerSeries {
	type acc struct {
		sum [4]float64
		cnt [4]int
	}
	accs := make([]acc, n)
	for _, p := range points {
		d := p.T.Sub(origin)
		if d < -interval || n == 0 {
			continue
		}
		i := 0
		if d > 0 {
			i = int(d / interval)
		}
		if i >= n {
			if !foldTail || d > time.Duration(n+1)*interval {
				continue
			}
			i = n - 1
		}
		for k, v := range []*float64{p.RSSBytes, p.HeapBytes, p.OpenFDs, p.ActiveSessions} {
			if v != nil && !math.IsNaN(*v) {
				accs[i].sum[k] += *v
				accs[i].cnt[k]++
			}
		}
	}
	var out [4][]*float64
	any := [4]bool{}
	for k := 0; k < 4; k++ {
		out[k] = make([]*float64, n)
		for i := 0; i < n; i++ {
			if accs[i].cnt[k] > 0 {
				out[k][i] = ptr(accs[i].sum[k] / float64(accs[i].cnt[k]))
				any[k] = true
			}
		}
	}
	ss := ServerSeries{RSSBytes: out[0]}
	// Optional series are omitted entirely when the sampler never produced them.
	if any[1] {
		ss.HeapBytes = out[1]
	}
	if any[2] {
		ss.OpenFDs = out[2]
	}
	if any[3] {
		ss.ActiveSessions = out[3]
	}
	return ss
}

// Times returns the bucket start times in seconds: 0, iv, 2iv, ...
func Times(n int, interval time.Duration) []float64 {
	t := make([]float64, n)
	for i := range t {
		t[i] = math.Round(float64(i)*interval.Seconds()*1000) / 1000
	}
	return t
}

func ptr(v float64) *float64 { return &v }

func round(v float64) int64 { return int64(math.Round(v)) }

func ratio(a, b int64) float64 {
	if b <= 0 {
		return 0
	}
	r := float64(a) / float64(b)
	if r > 1 {
		return 1
	}
	return r
}

// sanitizeKey makes an error_type usable as a byErrorType key (^[a-z][a-z0-9_]*$).
func sanitizeKey(k string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(k) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "e_" + s
	}
	return s
}
