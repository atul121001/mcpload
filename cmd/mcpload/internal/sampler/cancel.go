package sampler

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Server-side cancellation metrics, as exposed by the TypeScript demo server
// (demo-servers/ts-server). Any server can expose the same names to get the
// server side of the cancellation verdict.
const (
	PromCancelledTotal  = "mcp_cancelled_total"           // counter{tool}: calls cancelled while running
	PromWorkAfterCancel = "mcp_work_after_cancel_seconds" // histogram{tool}: how long they kept running
	PromCancelInflight  = "mcp_cancelled_inflight"        // gauge: cancelled calls still running
)

// Histogram is one Prometheus histogram series: cumulative bucket counts by
// upper bound (le), plus count and sum.
type Histogram struct {
	Buckets    map[float64]float64
	Count, Sum float64
}

// Sub returns h - o (the observations made between two scrapes).
func (h Histogram) Sub(o Histogram) Histogram {
	out := Histogram{Buckets: map[float64]float64{}, Count: h.Count - o.Count, Sum: h.Sum - o.Sum}
	for le, v := range h.Buckets {
		out.Buckets[le] = v - o.Buckets[le]
	}
	return out
}

// Add returns h + o.
func (h Histogram) Add(o Histogram) Histogram {
	out := Histogram{Buckets: map[float64]float64{}, Count: h.Count + o.Count, Sum: h.Sum + o.Sum}
	for le, v := range h.Buckets {
		out.Buckets[le] += v
	}
	for le, v := range o.Buckets {
		out.Buckets[le] += v
	}
	return out
}

// Quantile estimates the q-quantile (0..1) like PromQL histogram_quantile:
// linear interpolation inside the bucket that holds the rank. Observations
// in the +Inf bucket return the highest finite bound. NaN without
// observations.
func (h Histogram) Quantile(q float64) float64 {
	les := make([]float64, 0, len(h.Buckets))
	for le := range h.Buckets {
		les = append(les, le)
	}
	sort.Float64s(les)
	if len(les) == 0 {
		return math.NaN()
	}
	total := h.Buckets[les[len(les)-1]]
	if !math.IsInf(les[len(les)-1], 1) && h.Count > total {
		total = h.Count
	}
	if total <= 0 {
		return math.NaN()
	}
	rank := q * total
	prevLE, prevN := 0.0, 0.0
	for _, le := range les {
		n := h.Buckets[le]
		if n >= rank {
			if math.IsInf(le, 1) {
				return prevLE
			}
			if n == prevN {
				return le
			}
			return prevLE + (le-prevLE)*(rank-prevN)/(n-prevN)
		}
		prevLE, prevN = le, n
	}
	return prevLE
}

// CancelSnapshot is one reading of a server's cancellation metrics. Present
// is set when the server exposes mcp_cancelled_total or
// mcp_work_after_cancel_seconds at all.
type CancelSnapshot struct {
	Present   bool
	Cancelled map[string]float64   // by tool label ("" when unlabelled)
	Work      map[string]Histogram // by tool label
	Inflight  *float64
}

// InflightZero reports whether no cancelled call is still running (true when
// the gauge is not exposed).
func (s CancelSnapshot) InflightZero() bool { return s.Inflight == nil || *s.Inflight <= 0 }

// ScrapeCancel GETs a Prometheus text endpoint and reads its cancellation
// metrics.
func ScrapeCancel(ctx context.Context, url string) (CancelSnapshot, error) {
	hc := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return CancelSnapshot{}, err
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4")
	resp, err := hc.Do(req)
	if err != nil {
		return CancelSnapshot{}, fmt.Errorf("scrape %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return CancelSnapshot{}, fmt.Errorf("scrape %s: HTTP %d", url, resp.StatusCode)
	}
	return ParseCancelText(resp.Body)
}

// ParseCancelText reads the cancellation metrics from Prometheus text format.
// Other metrics are ignored.
func ParseCancelText(r io.Reader) (CancelSnapshot, error) {
	s := CancelSnapshot{Cancelled: map[string]float64{}, Work: map[string]Histogram{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	ln := 0
	for sc.Scan() {
		ln++
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "# TYPE ") {
			// A labelled metric has no samples before its first observation,
			// but its TYPE line already says the server exposes it.
			if f := strings.Fields(line); len(f) >= 3 && (f[2] == PromCancelledTotal || f[2] == PromWorkAfterCancel) {
				s.Present = true
			}
			continue
		}
		if line == "" || line[0] == '#' || !strings.HasPrefix(line, "mcp_") {
			continue
		}
		name, rest, err := splitPromLine(line)
		if err != nil {
			return s, fmt.Errorf("line %d: %w", ln, err)
		}
		switch name {
		case PromCancelledTotal, PromCancelInflight,
			PromWorkAfterCancel + "_bucket", PromWorkAfterCancel + "_count", PromWorkAfterCancel + "_sum":
		default:
			continue
		}
		labels := parseLabels(line[len(name):])
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return s, fmt.Errorf("line %d: missing value", ln)
		}
		v, err := parsePromFloat(fields[0])
		if err != nil {
			return s, fmt.Errorf("line %d: %w", ln, err)
		}
		if math.IsNaN(v) {
			continue
		}
		tool := labels["tool"]
		switch name {
		case PromCancelledTotal:
			s.Present = true
			s.Cancelled[tool] += v
		case PromCancelInflight:
			s.Inflight = fp(v)
		default:
			s.Present = true
			h := s.Work[tool]
			if h.Buckets == nil {
				h.Buckets = map[float64]float64{}
			}
			switch {
			case strings.HasSuffix(name, "_bucket"):
				le, err := parsePromFloat(labels["le"])
				if err != nil {
					return s, fmt.Errorf("line %d: bucket le: %w", ln, err)
				}
				h.Buckets[le] += v
			case strings.HasSuffix(name, "_count"):
				h.Count += v
			default:
				h.Sum += v
			}
			s.Work[tool] = h
		}
	}
	return s, sc.Err()
}

// parseLabels parses a leading {k="v",...} label set (empty map without one).
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	if !strings.HasPrefix(s, "{") {
		return out
	}
	i := 1
	for i < len(s) {
		for i < len(s) && (s[i] == ',' || s[i] == ' ') {
			i++
		}
		if i >= len(s) || s[i] == '}' {
			break
		}
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			break
		}
		key := strings.TrimSpace(s[i : i+eq])
		i += eq + 1
		if i >= len(s) || s[i] != '"' {
			break
		}
		i++
		var b strings.Builder
		for i < len(s) && s[i] != '"' {
			if s[i] == '\\' && i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					b.WriteByte('\n')
				default:
					b.WriteByte(s[i])
				}
			} else {
				b.WriteByte(s[i])
			}
			i++
		}
		i++ // closing quote
		out[key] = b.String()
	}
	return out
}

// CancelDelta is the server-side cancellation activity between two scrapes.
type CancelDelta struct {
	Cancelled float64
	ByTool    map[string]float64   // cancelled per tool
	Work      Histogram            // all tools
	WorkBy    map[string]Histogram // per tool
}

// Delta returns end - start (counters reset by a server restart count from 0).
func (end CancelSnapshot) Delta(start CancelSnapshot) CancelDelta {
	d := CancelDelta{ByTool: map[string]float64{}, Work: Histogram{Buckets: map[float64]float64{}}, WorkBy: map[string]Histogram{}}
	for tool, v := range end.Cancelled {
		dv := v - start.Cancelled[tool]
		if dv < 0 {
			dv = v
		}
		if dv > 0 {
			d.ByTool[tool] = dv
			d.Cancelled += dv
		}
	}
	for tool, h := range end.Work {
		dh := h
		if s, ok := start.Work[tool]; ok && s.Count <= h.Count {
			dh = h.Sub(s)
		}
		if dh.Count > 0 {
			d.WorkBy[tool] = dh
			d.Work = d.Work.Add(dh)
		}
	}
	return d
}
