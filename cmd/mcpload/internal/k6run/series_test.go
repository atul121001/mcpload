package k6run

import (
	"testing"
	"time"
)

func parseDropped(t *testing.T) *Aggregator {
	t.Helper()
	a, err := ParseFile("testdata/dropped.ndjson", 10*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func vals(s []*float64) []any {
	out := make([]any, len(s))
	for i, v := range s {
		if v == nil {
			out[i] = nil
		} else {
			out[i] = *v
		}
	}
	return out
}

func eqSeries(t *testing.T, name string, got []*float64, want ...any) {
	t.Helper()
	g := vals(got)
	if len(g) != len(want) {
		t.Errorf("%s = %v, want %v", name, g, want)
		return
	}
	for i := range want {
		switch w := want[i].(type) {
		case nil:
			if g[i] != nil {
				t.Errorf("%s = %v, want %v", name, g, want)
				return
			}
		case float64:
			if g[i] == nil || !near(g[i].(float64), w) {
				t.Errorf("%s = %v, want %v", name, g, want)
				return
			}
		}
	}
}

func TestIterationsDroppedToolErrors(t *testing.T) {
	s := parseDropped(t).Summary()
	if s.Iterations != 4 || s.DroppedIterations != 4 || s.ToolErrors != 1 {
		t.Errorf("summary = %+v", s)
	}
	if s.Reqs != 7 || s.Errors != 2 || s.ByErrorType["timeout"] != 1 || s.ByErrorType["tool_iserror"] != 1 {
		t.Errorf("summary = %+v", s)
	}
	// The run.ndjson fixture has no k6 iteration metrics.
	if s := parseFixture(t, nil).Summary(); s.Iterations != 0 || s.DroppedIterations != 0 || s.ToolErrors != 1 {
		t.Errorf("run.ndjson summary = %+v", s)
	}
}

func TestSeriesDroppedAndTools(t *testing.T) {
	a := parseDropped(t)
	c := a.Series(3, true)
	eqSeries(t, "dropped", c.DroppedIterations, 2.0, 1.0, 1.0)
	// search: bucket0 successes 10,30 (the 500 ms timeout is excluded) -> p95 29.
	eqSeries(t, "search", c.Tools["search"], 29.0, 50.0, 70.0)
	eqSeries(t, "slow", c.Tools["slow"], nil, 200.0, nil)
	eqSeries(t, "flaky", c.Tools["flaky"], nil, nil, nil)
	if len(c.Tools) != 3 {
		t.Errorf("tools = %v", c.Tools)
	}
	// Client p95 still covers every request, failed ones included.
	if v := val(t, c.P95Ms[0]); v < 400 {
		t.Errorf("client p95 bucket0 = %v, want failed calls included", v)
	}
	eqSeries(t, "rps", c.RPS, 0.4, 0.2, 0.1)

	// Tools stats: success-only percentiles, all calls counted.
	for _, ts := range a.Tools() {
		if ts.Name == "search" && (ts.Reqs != 5 || ts.Errors != 1 || ts.Max != 70 || ts.P50 != 40) {
			t.Errorf("search = %+v", ts)
		}
	}
}

func TestSeriesTrimmedTail(t *testing.T) {
	a := parseDropped(t)
	full := a.Series(3, true)
	// Trimmed: the samples of bucket 2 are left out; kept buckets unchanged.
	c := a.Series(2, false)
	eqSeries(t, "rps", c.RPS, val(t, full.RPS[0]), val(t, full.RPS[1]))
	eqSeries(t, "dropped", c.DroppedIterations, 2.0, 1.0)
	eqSeries(t, "search", c.Tools["search"], 29.0, 50.0)
	// Folded (clock skew): bucket 2 merges into bucket 1.
	f := a.Series(2, true)
	eqSeries(t, "dropped folded", f.DroppedIterations, 2.0, 2.0)
	eqSeries(t, "rps folded", f.RPS, 0.4, 0.3)
	eqSeries(t, "search folded", f.Tools["search"], 29.0, 69.0)
}

func TestKeepBuckets(t *testing.T) {
	iv := 10 * time.Second
	cases := []struct {
		d       float64
		n       int
		trimmed bool
	}{
		{0, 1, false},
		{3, 1, false}, // a single bucket is always kept
		{20, 2, false},
		{20.003, 2, true}, // 0.003 s tail
		{24.9, 2, true},
		{25, 3, false}, // exactly half an interval is kept
		{29, 3, false},
		{61.2, 6, true},
	}
	for _, c := range cases {
		n, tr := KeepBuckets(c.d, iv)
		if n != c.n || tr != c.trimmed {
			t.Errorf("KeepBuckets(%v) = %d,%v want %d,%v", c.d, n, tr, c.n, c.trimmed)
		}
	}
}

func TestAlignServerTrimmed(t *testing.T) {
	origin := time.Unix(1000, 0)
	pt := func(s float64, v float64) ServerPoint {
		return ServerPoint{T: origin.Add(time.Duration(s * float64(time.Second))), RSSBytes: ptr(v)}
	}
	pts := []ServerPoint{pt(1, 10), pt(11, 20), pt(21, 99)}
	eqSeries(t, "folded", AlignServerN(pts, origin, 10*time.Second, 2, true).RSSBytes, 10.0, 59.5)
	eqSeries(t, "trimmed", AlignServerN(pts, origin, 10*time.Second, 2, false).RSSBytes, 10.0, 20.0)
}
