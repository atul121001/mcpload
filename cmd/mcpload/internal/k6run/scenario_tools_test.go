package k6run

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestScenarioToolsSplitsByScenario(t *testing.T) {
	pt := func(sc, tool string, v float64, errType string) string {
		e := ""
		if errType != "" {
			e = `,"error_type":"` + errType + `"`
		}
		return `{"metric":"mcp_req_duration","type":"Point","data":{"time":"2026-10-03T00:00:01Z","value":` +
			strconv.FormatFloat(v, 'f', -1, 64) +
			`,"tags":{"method":"tools/call","tool":"` + tool + `","scenario":"` + sc + `"` + e + `}}}`
	}
	lines := []string{
		pt("solo", "fast", 2, ""), pt("solo", "fast", 4, ""),
		pt("mixed", "fast", 300, ""), pt("mixed", "slow", 310, ""),
		pt("mixed", "fast", 9, "timeout"), // failed calls are left out
	}
	origin, _ := time.Parse(time.RFC3339, "2026-10-03T00:00:00Z")
	a := NewAggregator(origin, 10*time.Second, nil)
	if err := a.Read(strings.NewReader(strings.Join(lines, "\n"))); err != nil {
		t.Fatal(err)
	}
	solo, mixed := a.ScenarioTools("solo"), a.ScenarioTools("mixed")
	if solo["fast"].Calls != 2 || solo["fast"].P95 < 3.9 || len(solo) != 1 {
		t.Errorf("solo = %+v", solo)
	}
	if mixed["fast"].Calls != 1 || mixed["fast"].P95 != 300 || mixed["slow"].Calls != 1 {
		t.Errorf("mixed = %+v", mixed)
	}
	if a.ScenarioTools("none") != nil {
		t.Error("unknown scenario should be nil")
	}
}
