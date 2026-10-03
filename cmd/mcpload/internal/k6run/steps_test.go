package k6run

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestStepsSplitByStepTag(t *testing.T) {
	pt := func(sec int, metric, step, method, tool, errType string, v float64) string {
		tags := `"method":"` + method + `"`
		if tool != "" {
			tags += `,"tool":"` + tool + `"`
		}
		if step != "" {
			tags += `,"step":"` + step + `"`
		}
		if errType != "" {
			tags += `,"error_type":"` + errType + `"`
		}
		return fmt.Sprintf(`{"metric":%q,"type":"Point","data":{"time":"2026-10-03T00:00:%02dZ","value":%g,"tags":{%s}}}`, metric, sec, v, tags)
	}
	call := func(sec int, step, tool string, ms float64, errType string) []string {
		l := []string{pt(sec, MetricReqs, step, "tools/call", tool, errType, 1), pt(sec, MetricReqDuration, step, "tools/call", tool, errType, ms)}
		if errType != "" {
			l = append(l, pt(sec, MetricErrors, step, "tools/call", tool, errType, 1))
		}
		return l
	}
	var lines []string
	lines = append(lines, pt(5, MetricConnectDuration, "5", "initialize", "", "", 12))
	lines = append(lines, call(6, "5", "fast", 3, "")...)
	lines = append(lines, call(9, "5", "fast", 5, "")...)
	lines = append(lines, call(30, "10", "fast", 40, "")...)
	lines = append(lines, call(31, "10", "slow", 900, "timeout")...)
	lines = append(lines, pt(32, MetricConnectDuration, "10", "initialize", "", "http", 2))
	lines = append(lines, call(27, "", "fast", 7, "")...)                // ramp: untagged
	lines = append(lines, pt(40, MetricIterations, "10", "", "", "", 1)) // k6 metric: no span
	lines = append(lines, call(33, "warmup", "fast", 7, "")...)          // not a VU count
	origin, _ := time.Parse(time.RFC3339, "2026-10-03T00:00:00Z")
	a := NewAggregator(origin, 10*time.Second, nil)
	if err := a.Read(strings.NewReader(strings.Join(lines, "\n"))); err != nil {
		t.Fatal(err)
	}
	st := a.Steps()
	if len(st) != 2 || st[0].VUs != 5 || st[1].VUs != 10 {
		t.Fatalf("steps = %+v", st)
	}
	s5, s10 := st[0], st[1]
	if s5.Reqs != 2 || s5.Errors != 0 || len(s5.Tools) != 1 || s5.Tools[0].P95 < 4.8 || s5.Connects != 1 || s5.ConnectP95 == nil || *s5.ConnectP95 != 12 {
		t.Errorf("step 5 = %+v", s5)
	}
	if s5.First.Sub(origin) != 5*time.Second || s5.Last.Sub(origin) != 9*time.Second {
		t.Errorf("step 5 span %v..%v", s5.First, s5.Last)
	}
	if s10.Reqs != 2 || s10.Errors != 1 || s10.Connects != 1 || s10.ConnectErrors != 1 || s10.ConnectP95 != nil {
		t.Errorf("step 10 = %+v", s10)
	}
	if s10.Last.Sub(origin) != 32*time.Second {
		t.Errorf("step 10 ends %v; iterations must not extend it", s10.Last.Sub(origin))
	}
	slow := s10.Tools[1]
	if slow.Name != "slow" || slow.Reqs != 1 || slow.Errors != 1 || slow.ErrorRate != 1 || slow.P95 != 0 {
		t.Errorf("slow = %+v", slow)
	}
	if NewAggregator(origin, time.Second, nil).Steps() != nil {
		t.Error("no step tags should give nil")
	}
}

func TestStepLevels(t *testing.T) {
	o, err := ParseOptions([]byte(`{"scenarios":{"steps":{"executor":"ramping-vus","stages":[
		{"duration":"5s","target":5},{"duration":"20s","target":5},{"duration":"5s","target":10},{"duration":"20s","target":10}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := o.StepLevels("steps"); fmt.Sprint(got) != "[5 10]" {
		t.Errorf("levels = %v", got)
	}
	if o.StepLevels("other") != nil || (*Options)(nil).StepLevels("steps") != nil {
		t.Error("missing scenario should give nil")
	}
}
