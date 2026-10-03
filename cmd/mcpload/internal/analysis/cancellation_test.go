package analysis

import (
	"strings"
	"testing"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

func cancelled(cancels, late, failed int64, server *report.CancelServer) *report.Cancellation {
	c := &report.Cancellation{Cancels: cancels, LateResponses: late,
		ByOutcome: map[string]int64{"cancelled": cancels - late - failed, "late_response": late, "send_failed": failed},
		ByReason:  map[string]int64{"client": cancels}, ByTool: map[string]int64{"slow": cancels},
		SendMs: &report.Latency{Count: cancels, P50: 1.5, P95: 3, P99: 4, Max: 5}, Server: server}
	if late > 0 {
		c.LateAfterMs = &report.Latency{Count: late, P50: 1850, P95: 1900, P99: 1950, Max: 2000}
	}
	return c
}

func TestCancellationVerdict(t *testing.T) {
	cases := []struct {
		name   string
		c      *report.Cancellation
		worst  *CancelTool
		status string
		msg    []string
	}{
		{name: "nothing cancelled", c: nil, status: report.StatusSkipped, msg: []string{"no call was cancelled"}},
		{name: "only completed", c: &report.Cancellation{ByOutcome: map[string]int64{"completed": 5}}, status: report.StatusSkipped},
		{name: "server honours", c: cancelled(480, 0, 0, &report.CancelServer{Cancelled: 470, Observed: 470, WorkAfterCancelP50Ms: report.F(2.5), WorkAfterCancelP95Ms: report.F(8)}),
			status: report.StatusPass, msg: []string{"stopped cancelled work within a median 2.5 ms (470 cancels seen by the server of 480 cancels sent)", "a median 1.5 ms to send", "0 late responses"}},
		{name: "server keeps working", c: cancelled(480, 470, 0, &report.CancelServer{Cancelled: 480, Observed: 480, WorkAfterCancelP50Ms: report.F(2100), WorkAfterCancelP95Ms: report.F(2400), WorkAfterCancelTotalS: 1008}),
			worst: &CancelTool{Name: "slow", Count: 480, P50Ms: 2100}, status: report.StatusFail,
			msg: []string{"The server kept running `slow` for a median 2.1 s after 480 cancels: cancelled work still uses capacity (1008.0 s", "470 late responses (a median 1.85 s after the cancel)"}},
		{name: "a little work after cancel", c: cancelled(10, 0, 0, &report.CancelServer{Cancelled: 10, Observed: 10, WorkAfterCancelP50Ms: report.F(150), WorkAfterCancelP95Ms: report.F(200)}),
			status: report.StatusWarn, msg: []string{"kept running cancelled calls for a median 150 ms after 10 cancels"}},
		{name: "server saw none", c: cancelled(10, 0, 0, &report.CancelServer{}), status: report.StatusWarn, msg: []string{"registered none of the 10 cancels"}},
		{name: "unmeasured, late responses", c: cancelled(100, 60, 0, nil), status: report.StatusWarn,
			msg: []string{"60 of 100 cancels still got a response afterwards", "The server side was not measured"}},
		{name: "unmeasured, clean", c: cancelled(100, 2, 0, nil), status: report.StatusPass, msg: []string{"100 cancels sent.", "not measured", "2 late responses"}},
		{name: "send failures", c: cancelled(10, 0, 3, nil), status: report.StatusWarn, msg: []string{"3 cancels could not be sent"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := CancellationVerdict(tc.c, tc.worst)
			if v.ID != report.VerdictCancellation || v.Status != tc.status {
				t.Fatalf("status %s, want %s: %s", v.Status, tc.status, v.Message)
			}
			for _, m := range tc.msg {
				if !strings.Contains(v.Message, m) {
					t.Errorf("message %q lacks %q", v.Message, m)
				}
			}
		})
	}
}
