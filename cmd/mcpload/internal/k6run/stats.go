package k6run

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Percentile uses k6's trend-sink method (linear interpolation between the
// closest ranks). sorted must be sorted ascending. pct is 0..1.
func Percentile(sorted []float64, pct float64) float64 {
	switch len(sorted) {
	case 0:
		return 0
	case 1:
		return sorted[0]
	}
	i := pct * float64(len(sorted)-1)
	lo := sorted[int(math.Floor(i))]
	hi := sorted[int(math.Ceil(i))]
	return lo + (hi-lo)*(i-math.Floor(i))
}

// Selector is a parsed k6 metric key such as mcp_req_duration{tool:search,method:tools/call}.
type Selector struct {
	Key    string
	Metric string
	Tags   map[string]string
}

// ParseSelector parses a k6 metric key with an optional tag selector.
func ParseSelector(key string) (Selector, error) {
	s := Selector{Key: key, Tags: map[string]string{}}
	i := strings.IndexByte(key, '{')
	if i < 0 {
		s.Metric = strings.TrimSpace(key)
		return s, nil
	}
	if !strings.HasSuffix(key, "}") {
		return s, fmt.Errorf("bad metric selector %q", key)
	}
	s.Metric = strings.TrimSpace(key[:i])
	body := key[i+1 : len(key)-1]
	for _, part := range strings.Split(body, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, ":")
		if !ok {
			return s, fmt.Errorf("bad tag %q in selector %q", part, key)
		}
		s.Tags[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return s, nil
}

// Matches reports whether a sample of metric with tags is part of the selector.
func (s Selector) Matches(metric string, tags map[string]string) bool {
	if metric != s.Metric {
		return false
	}
	for k, v := range s.Tags {
		if tags[k] != v {
			return false
		}
	}
	return true
}

var exprRe = regexp.MustCompile(`^\s*([a-z]+(?:\(\s*[0-9.]+\s*\))?)\s*(<=|>=|===|==|!=|<|>)\s*(-?[0-9.eE+-]+)\s*$`)

// Expr is a parsed threshold expression like "p(95)<800".
type Expr struct {
	Agg   string // avg, min, max, med, count, rate, value, p(N)
	Op    string
	Value float64
}

// ParseExpr parses a k6 threshold expression.
func ParseExpr(s string) (Expr, error) {
	m := exprRe.FindStringSubmatch(s)
	if m == nil {
		return Expr{}, fmt.Errorf("unsupported threshold expression %q", s)
	}
	v, err := strconv.ParseFloat(m[3], 64)
	if err != nil {
		return Expr{}, fmt.Errorf("threshold %q: %w", s, err)
	}
	return Expr{Agg: strings.ReplaceAll(m[1], " ", ""), Op: m[2], Value: v}, nil
}

// Holds evaluates observed <op> value.
func (e Expr) Holds(observed float64) bool {
	switch e.Op {
	case "<":
		return observed < e.Value
	case "<=":
		return observed <= e.Value
	case ">":
		return observed > e.Value
	case ">=":
		return observed >= e.Value
	case "==", "===":
		return observed == e.Value
	case "!=":
		return observed != e.Value
	}
	return false
}

// series collects the samples of one tracked selector.
type series struct {
	sel    Selector
	values []float64
	sorted bool
}

// aggregate computes the value an expression aggregates, for a metric of the
// given k6 type (trend, rate, counter, gauge). ok is false with no samples.
func (s *series) aggregate(metricType, agg string, durationS float64) (float64, bool) {
	n := len(s.values)
	if n == 0 {
		return 0, false
	}
	if !s.sorted && metricType == "trend" {
		sort.Float64s(s.values)
		s.sorted = true
	}
	sum := 0.0
	for _, v := range s.values {
		sum += v
	}
	switch metricType {
	case "trend":
		switch {
		case agg == "avg":
			return sum / float64(n), true
		case agg == "min":
			return s.values[0], true
		case agg == "max":
			return s.values[n-1], true
		case agg == "med":
			return Percentile(s.values, 0.5), true
		case agg == "count":
			return float64(n), true
		case strings.HasPrefix(agg, "p("):
			p, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(agg, "p("), ")"), 64)
			if err != nil {
				return 0, false
			}
			return Percentile(s.values, p/100), true
		}
	case "rate":
		if agg == "rate" {
			nz := 0
			for _, v := range s.values {
				if v != 0 {
					nz++
				}
			}
			return float64(nz) / float64(n), true
		}
	case "counter":
		switch agg {
		case "count":
			return sum, true
		case "rate":
			if durationS > 0 {
				return sum / durationS, true
			}
			return 0, true
		}
	case "gauge":
		switch agg {
		case "value":
			return s.values[n-1], true
		case "min", "max":
			mn, mx := s.values[0], s.values[0]
			for _, v := range s.values {
				mn, mx = math.Min(mn, v), math.Max(mx, v)
			}
			if agg == "min" {
				return mn, true
			}
			return mx, true
		}
	}
	return 0, false
}
