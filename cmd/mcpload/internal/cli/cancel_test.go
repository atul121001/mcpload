package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/k6run"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/sampler"
)

func promCancel(cancelled, fast, inf int, sum float64, inflight int) string {
	return fmt.Sprintf(`mcp_cancelled_total{tool="slow"} %d
mcp_work_after_cancel_seconds_bucket{le="0.1",tool="slow"} %d
mcp_work_after_cancel_seconds_bucket{le="2.5",tool="slow"} %d
mcp_work_after_cancel_seconds_bucket{le="+Inf",tool="slow"} %d
mcp_work_after_cancel_seconds_sum{tool="slow"} %g
mcp_work_after_cancel_seconds_count{tool="slow"} %d
mcp_cancelled_inflight %d
`, cancelled, fast, inf, inf, sum, inf, inflight)
}

// The end scrape waits for cancelled calls still running, then reports the
// work since the start scrape.
func TestServerCancellation(t *testing.T) {
	var scrapes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if scrapes.Add(1) == 1 {
			fmt.Fprint(w, promCancel(25, 0, 20, 30, 5)) // 5 cancelled calls still running
			return
		}
		fmt.Fprint(w, promCancel(25, 0, 25, 42, 0))
	}))
	defer srv.Close()
	start, err := sampler.ParseCancelText(strings.NewReader(promCancel(5, 0, 5, 9, 0)))
	if err != nil {
		t.Fatal(err)
	}
	old := cancelSettle
	cancelSettle = 5 * time.Second
	defer func() { cancelSettle = old }()
	s, worst := serverCancellation(srv.URL, start, t.Logf)
	if scrapes.Load() != 2 {
		t.Errorf("scrapes = %d, want 2 (wait for inflight 0)", scrapes.Load())
	}
	if s == nil || s.Cancelled != 20 || s.Observed != 20 || s.WorkAfterCancelTotalS != 33 || s.WorkAfterCancelP50Ms == nil ||
		*s.WorkAfterCancelP50Ms < 1000 || *s.WorkAfterCancelP50Ms > 1400 {
		t.Fatalf("server = %+v", s)
	}
	if worst == nil || worst.Name != "slow" || worst.Count != 20 {
		t.Errorf("worst = %+v", worst)
	}

	c := toCancellation(&k6run.CancelStats{Cancels: 20, ByOutcome: map[string]int64{"late_response": 18, "cancelled": 2, "completed": 7},
		ByReason: map[string]int64{"client": 20}, ByTool: map[string]int64{"slow": 20}, SendMs: k6run.Latency{Count: 20, P50: 1, P95: 2, P99: 2, Max: 3}})
	if c.LateResponses != 18 || c.SendMs == nil || c.LateAfterMs != nil {
		t.Errorf("cancellation = %+v", c)
	}
}
