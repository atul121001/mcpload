package cli

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/analysis"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/k6run"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/sampler"
)

// cancelSettle bounds how long mcpload waits after k6 for cancelled calls
// still running on the server (mcp_cancelled_inflight) before it reads the
// server's cancellation counters.
var cancelSettle = 15 * time.Second

// toCancellation converts the client-side cancellation stats.
func toCancellation(cs *k6run.CancelStats) *report.Cancellation {
	c := &report.Cancellation{Cancels: cs.Cancels, ByOutcome: cs.ByOutcome, ByReason: cs.ByReason, ByTool: cs.ByTool,
		LateResponses: cs.ByOutcome[k6run.CancelOutcomeLateResponse]}
	if c.LateResponses > c.Cancels {
		c.LateResponses = c.Cancels
	}
	if cs.SendMs.Count > 0 {
		l := toLatency(cs.SendMs)
		c.SendMs = &l
	}
	if cs.LateMs.Count > 0 {
		l := toLatency(cs.LateMs)
		c.LateAfterMs = &l
	}
	return c
}

// serverCancellation reads the server's cancellation counters after the run
// (waiting up to cancelSettle for cancelled calls that are still running)
// and returns the activity since start, plus the tool whose cancelled calls
// ran longest. nil when the end scrape fails.
func serverCancellation(promURL string, start sampler.CancelSnapshot, logf func(string, ...any)) (*report.CancelServer, *analysis.CancelTool) {
	deadline := time.Now().Add(cancelSettle)
	var end sampler.CancelSnapshot
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		snap, err := sampler.ScrapeCancel(ctx, promURL)
		cancel()
		if err != nil {
			logf("warning: reading the server's cancellation metrics failed: %v", err)
			return nil, nil
		}
		end = snap
		if snap.InflightZero() || time.Now().After(deadline) {
			if !snap.InflightZero() {
				logf("warning: %.0f cancelled calls were still running on the server %s after k6 ended", *snap.Inflight, cancelSettle)
			}
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	d := end.Delta(start)
	s := &report.CancelServer{Cancelled: int64(math.Round(d.Cancelled)), Observed: int64(math.Round(d.Work.Count)),
		WorkAfterCancelTotalS: round3(math.Max(0, d.Work.Sum))}
	ms := func(q float64, h sampler.Histogram) *float64 {
		if v := h.Quantile(q); !math.IsNaN(v) {
			return report.F(round3(1000 * v))
		}
		return nil
	}
	s.WorkAfterCancelP50Ms, s.WorkAfterCancelP95Ms = ms(0.5, d.Work), ms(0.95, d.Work)
	var worst *analysis.CancelTool
	tools := make([]string, 0, len(d.WorkBy))
	for name := range d.WorkBy {
		tools = append(tools, name)
	}
	sort.Strings(tools)
	for _, name := range tools {
		h := d.WorkBy[name]
		p := ms(0.5, h)
		if p == nil || name == "" {
			continue
		}
		if worst == nil || *p > worst.P50Ms {
			worst = &analysis.CancelTool{Name: name, Count: int64(math.Round(h.Count)), P50Ms: *p}
		}
	}
	return s, worst
}
