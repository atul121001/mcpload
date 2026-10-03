package k6run

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSkewStats(t *testing.T) {
	pt := func(metric string, v float64, tags string) string {
		return `{"metric":"` + metric + `","type":"Point","data":{"time":"2026-10-03T00:00:01Z","value":` +
			strconv.FormatFloat(v, 'f', -1, 64) + `,"tags":{` + tags + `}}}`
	}
	lines := []string{
		pt(MetricSkewRequests, 1, `"op":"connect","outcome":"ok","replica":"new","protocol":"2026-07-28"`),
		pt(MetricSkewRequests, 1, `"op":"call","outcome":"fail","replica":"old"`),
		pt(MetricSkewRequests, 1, `"op":"call","outcome":"fail"`), // a timeout: no replica
		pt(MetricSkewFailures, 1, `"op":"call","kind":"fast","reason":"http_400"`),
		pt(MetricSkewFailures, 1, `"op":"call","kind":"hang","reason":"timeout"`),
		pt(MetricSkewFailureDuration, 2, `"op":"call","kind":"fast","reason":"http_400"`),
		pt(MetricSkewFailureDuration, 5000, `"op":"call","kind":"hang","reason":"timeout"`),
		pt(MetricSkewNegotiated, 1, `"protocol":"2026-07-28","replica":"new"`),
	}
	origin, _ := time.Parse(time.RFC3339, "2026-10-03T00:00:00Z")
	a := NewAggregator(origin, 10*time.Second, nil)
	if err := a.Read(strings.NewReader(strings.Join(lines, "\n"))); err != nil {
		t.Fatal(err)
	}
	s := a.Skew()
	if s == nil {
		t.Fatal("Skew() = nil")
	}
	if s.Requests != 3 || s.Replicas["new"] != 1 || s.Replicas["old"] != 1 || len(s.Replicas) != 2 {
		t.Errorf("requests %d replicas %v", s.Requests, s.Replicas)
	}
	if s.Failures["fast"]["http_400"] != 1 || s.Failures["hang"]["timeout"] != 1 {
		t.Errorf("failures %v", s.Failures)
	}
	if s.FailureMs["hang"].P50 != 5000 || s.FailureMs["fast"].Max != 2 {
		t.Errorf("failure ms %v", s.FailureMs)
	}
	if s.Negotiated["2026-07-28"]["new"] != 1 {
		t.Errorf("negotiated %v", s.Negotiated)
	}
}

func TestSkewStatsAbsent(t *testing.T) {
	if s := parseDropped(t).Skew(); s != nil {
		t.Errorf("Skew() = %+v, want nil for a run without version-skew metrics", s)
	}
}
