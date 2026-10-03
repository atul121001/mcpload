package sampler

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

const cancelScrape = `# HELP mcp_cancelled_total x
# TYPE mcp_cancelled_total counter
mcp_cancelled_total{tool="slow"} %d
# TYPE mcp_work_after_cancel_seconds histogram
mcp_work_after_cancel_seconds_bucket{le="0.1",tool="slow"} %d
mcp_work_after_cancel_seconds_bucket{le="1",tool="slow"} %d
mcp_work_after_cancel_seconds_bucket{le="2.5",tool="slow"} %d
mcp_work_after_cancel_seconds_bucket{le="+Inf",tool="slow"} %d
mcp_work_after_cancel_seconds_sum{tool="slow"} %g
mcp_work_after_cancel_seconds_count{tool="slow"} %d
mcp_cancelled_inflight %d
process_resident_memory_bytes 1e6
`

func scrape(t *testing.T, cancelled, b01, b1, b25, inf int, sum float64, inflight int) CancelSnapshot {
	t.Helper()
	s, err := ParseCancelText(strings.NewReader(fmt.Sprintf(cancelScrape, cancelled, b01, b1, b25, inf, sum, inf, inflight)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestParseCancelTextAndDelta(t *testing.T) {
	start := scrape(t, 10, 10, 10, 10, 10, 0.2, 0)
	if !start.Present || start.Cancelled["slow"] != 10 || start.Work["slow"].Count != 10 || start.Work["slow"].Buckets[math.Inf(1)] != 10 || !start.InflightZero() {
		t.Fatalf("start = %+v", start)
	}
	// 20 more cancels: 4 stopped within 0.1 s, 16 ran on for 1-2.5 s.
	end := scrape(t, 30, 14, 14, 30, 30, 30, 2)
	if end.InflightZero() {
		t.Error("inflight gauge not read")
	}
	d := end.Delta(start)
	if d.Cancelled != 20 || d.Work.Count != 20 || math.Abs(d.Work.Sum-29.8) > 1e-9 {
		t.Fatalf("delta = %+v", d)
	}
	// median: rank 10 lies in (1, 2.5] holding ranks 4..20 -> 1 + 1.5*6/16
	if q := d.Work.Quantile(0.5); math.Abs(q-(1+1.5*6.0/16)) > 1e-9 {
		t.Errorf("median = %v", q)
	}
	if q := d.Work.Quantile(0.1); q > 0.1 {
		t.Errorf("p10 = %v", q)
	}
	if !math.IsNaN((Histogram{}).Quantile(0.5)) {
		t.Error("empty histogram should give NaN")
	}

	none, err := ParseCancelText(strings.NewReader("process_resident_memory_bytes 1\n"))
	if err != nil || none.Present {
		t.Errorf("none = %+v, %v", none, err)
	}
	// prom-client before the first cancel: only HELP/TYPE lines.
	empty, err := ParseCancelText(strings.NewReader("# HELP mcp_cancelled_total x\n# TYPE mcp_cancelled_total counter\nmcp_cancelled_inflight 0\n"))
	if err != nil || !empty.Present || len(empty.Cancelled) != 0 {
		t.Errorf("empty = %+v, %v", empty, err)
	}
	if d := end.Delta(empty); d.Cancelled != 30 || d.Work.Count != 30 {
		t.Errorf("delta from empty = %+v", d)
	}
}

func TestParseLabels(t *testing.T) {
	got := parseLabels(`{a="x",le="+Inf", tool="we\"ird\\"} 3`)
	if got["a"] != "x" || got["le"] != "+Inf" || got["tool"] != `we"ird\` {
		t.Errorf("labels %v", got)
	}
}
