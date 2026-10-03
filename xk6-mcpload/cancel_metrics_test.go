package mcpload

import (
	"context"
	"testing"
	"time"

	"go.k6.io/k6/v2/metrics"

	"github.com/atul121001/mcpload/xk6-mcpload/client"
)

func TestParseCallOptions(t *testing.T) {
	for _, tc := range []struct {
		in   map[string]any
		want time.Duration
		bad  bool
	}{
		{map[string]any{}, 0, false},
		{map[string]any{"cancelAfterMs": int64(150)}, 150 * time.Millisecond, false},
		{map[string]any{"cancelAfterMs": 2.5}, 2500 * time.Microsecond, false},
		{map[string]any{"cancelAfterMs": "1s"}, time.Second, false},
		{map[string]any{"cancelAfterMs": nil}, 0, false},
		{map[string]any{"cancelAfterMs": int64(-1)}, 0, true},
		{map[string]any{"cancelAfterMs": true}, 0, true},
	} {
		co, err := parseCallOptions(tc.in, "options")
		if (err != nil) != tc.bad || co.CancelAfter != tc.want {
			t.Errorf("%v: got %v, %v", tc.in, co.CancelAfter, err)
		}
	}
}

func TestCancellationSamples(t *testing.T) {
	r := metrics.NewRegistry()
	m, err := registerMetrics(r)
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan metrics.SampleContainer, 20)
	e := newTestEmitter(t, context.Background(), r, m, ch, "a", nil)
	now := time.Now()
	// The cancelled call itself: counted in mcp_reqs, not as an error.
	e.OnRequest(client.RequestStats{Method: "tools/call", Tool: "slow", Status: 200, ErrorType: client.ErrCancelled, Start: now, Duration: 50 * time.Millisecond})
	e.OnCancel(client.CancelStats{Method: "tools/call", Tool: "slow", Reason: client.CancelReasonClient, Outcome: client.CancelOutcomeLateResponse,
		Status: 202, Start: now, Duration: 2 * time.Millisecond, LateAfter: 200 * time.Millisecond})
	e.OnCancel(client.CancelStats{Method: "tools/call", Tool: "slow", Reason: client.CancelReasonClient, Outcome: client.CancelOutcomeCompleted, Start: now})
	e.OnCancel(client.CancelStats{Method: "tools/call", Tool: "slow", Reason: client.CancelReasonTimeout, Outcome: client.CancelOutcomeSendFailed,
		ErrorType: client.ErrHTTP, Status: 500, Start: now, Duration: time.Millisecond})
	close(ch)
	got := map[string]int{}
	outcomes := map[string]int{}
	for sc := range ch {
		for _, s := range sc.GetSamples() {
			got[s.Metric.Name]++
			if s.Metric.Name == "mcp_cancellations" {
				o, _ := s.Tags.Get("outcome")
				outcomes[o]++
				if _, has := s.Tags.Get("reason"); !has {
					t.Fatal("mcp_cancellations without a reason tag")
				}
			}
		}
	}
	want := map[string]int{"mcp_reqs": 1, "mcp_req_duration": 1, "mcp_errors": 0, "mcp_tool_error_rate": 0,
		"mcp_cancellations": 3, "mcp_cancel_duration": 1, "mcp_cancel_late_response": 1}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: %d samples, want %d (all: %v)", k, got[k], v, got)
		}
	}
	if outcomes["late_response"] != 1 || outcomes["completed"] != 1 || outcomes["send_failed"] != 1 {
		t.Fatalf("outcomes %v", outcomes)
	}
}
