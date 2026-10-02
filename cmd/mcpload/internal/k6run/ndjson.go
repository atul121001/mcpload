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
	durations []float64
}

type toolAgg struct {
	reqs, errors float64
	durations    []float64
}

// Aggregator folds NDJSON points into report aggregates.
type Aggregator struct {
	Origin   time.Time
	Interval time.Duration

	buckets     []bucket
	tools       map[string]*toolAgg
	reqs        float64
	errors      float64
	byErrorType map[string]float64
	protoOK     map[string]int
	protoAny    map[string]int
	metricTypes map[string]string
	tracked     []*series
	first, last time.Time
	points      int
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
		if tags["method"] == methodToolsCall && tags["tool"] != "" {
			ta := a.tool(tags["tool"])
			ta.durations = append(ta.durations, v)
		}
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
}

// Summary returns the totals over the whole run.
func (a *Aggregator) Summary() Summary {
	s := Summary{Reqs: round(a.reqs), Errors: round(a.errors), ByErrorType: map[string]int64{}}
	for k, v := range a.byErrorType {
		s.ByErrorType[sanitizeKey(k)] += round(v)
	}
	s.ErrorRate = ratio(s.Errors, s.Reqs)
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
		if ts.Reqs < int64(len(t.durations)) {
			ts.Reqs = int64(len(t.durations))
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

// ClientSeries is report.series.client; each slice has n entries.
type ClientSeries struct {
	P95Ms, ErrorRate, RPS []*float64
}

// Buckets is the number of client buckets that saw data.
func (a *Aggregator) Buckets() int { return len(a.buckets) }

// Client returns n buckets of client series. p95 and error rate are null for
// buckets without requests; rps is 0 there.
func (a *Aggregator) Client(n int) ClientSeries {
	cs := ClientSeries{
		P95Ms:     make([]*float64, n),
		ErrorRate: make([]*float64, n),
		RPS:       make([]*float64, n),
	}
	iv := a.Interval.Seconds()
	for i := 0; i < n; i++ {
		var b bucket
		if i < len(a.buckets) {
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
	}
	// Samples past the last bucket (clock skew at shutdown) are folded into it.
	if n > 0 && len(a.buckets) > n {
		var reqs, errs float64
		var d []float64
		for _, b := range a.buckets[n-1:] {
			reqs += b.reqs
			errs += b.errors
			d = append(d, b.durations...)
		}
		if len(d) > 0 {
			sort.Float64s(d)
			cs.P95Ms[n-1] = ptr(Percentile(d, 0.95))
		}
		if reqs > 0 {
			cs.ErrorRate[n-1] = ptr(math.Min(1, errs/reqs))
		}
		cs.RPS[n-1] = ptr(reqs / iv)
	}
	return cs
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
// points after the last bucket go to the last one.
func AlignServer(points []ServerPoint, origin time.Time, interval time.Duration, n int) ServerSeries {
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
			if d > time.Duration(n+1)*interval {
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
