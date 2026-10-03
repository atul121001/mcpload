package k6run

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCancellations(t *testing.T) {
	pt := func(metric, tags string, v float64) string {
		return fmt.Sprintf(`{"metric":%q,"type":"Point","data":{"time":"2026-10-03T00:00:05Z","value":%g,"tags":{%s}}}`, metric, v, tags)
	}
	lines := []string{
		pt(MetricCancellations, `"tool":"slow","reason":"client","outcome":"cancelled"`, 1),
		pt(MetricCancellations, `"tool":"slow","reason":"client","outcome":"late_response"`, 1),
		pt(MetricCancellations, `"tool":"slow","reason":"client","outcome":"late_response"`, 1),
		pt(MetricCancellations, `"tool":"fast","reason":"client","outcome":"completed"`, 1),
		pt(MetricCancellations, `"tool":"big","reason":"timeout","outcome":"send_failed","error_type":"http"`, 1),
		pt(MetricCancelDuration, `"tool":"slow"`, 2),
		pt(MetricCancelDuration, `"tool":"slow"`, 4),
		pt(MetricCancelDuration, `"tool":"slow"`, 6),
		pt(MetricCancelLate, `"tool":"slow"`, 150),
		pt(MetricCancelLate, `"tool":"slow"`, 250),
	}
	origin, _ := time.Parse(time.RFC3339, "2026-10-03T00:00:00Z")
	a := NewAggregator(origin, 10*time.Second, nil)
	if a.Cancellations() != nil {
		t.Fatal("no samples should give nil")
	}
	if err := a.Read(strings.NewReader(strings.Join(lines, "\n"))); err != nil {
		t.Fatal(err)
	}
	c := a.Cancellations()
	if c == nil || c.Cancels != 4 {
		t.Fatalf("cancellations = %+v", c)
	}
	if fmt.Sprint(c.ByOutcome) != "map[cancelled:1 completed:1 late_response:2 send_failed:1]" ||
		fmt.Sprint(c.ByReason) != "map[client:3 timeout:1]" || fmt.Sprint(c.ByTool) != "map[big:1 slow:3]" {
		t.Errorf("splits: %v %v %v", c.ByOutcome, c.ByReason, c.ByTool)
	}
	if c.SendMs.Count != 3 || c.SendMs.P50 != 4 || c.LateMs.Count != 2 || c.LateMs.P50 != 200 {
		t.Errorf("latencies: send %+v late %+v", c.SendMs, c.LateMs)
	}
}
