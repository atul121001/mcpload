package analysis

import (
	"fmt"
	"strings"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

// Cancellation (report/schema/README.md "Verdict ids").
//
// Runs that cancel calls (CANCEL_RATE, or client-side timeouts, which the
// client cancels too) are judged on what the server does about a cancel:
//
//   - server side measured (Prometheus sampler, server exposes
//     mcp_work_after_cancel_seconds): fail when cancelled calls kept running
//     for a median >= CancelWorkFailMs after their cancel, warn when >=
//     CancelWorkWarnMs or when the server registered none of the cancels,
//     pass otherwise;
//   - not measured: warn when more than CancelLateShareWarn of the cancelled
//     calls still got a response afterwards (the server likely kept
//     working), pass otherwise, saying the server side was not measured.
//
// Cancels that could not be sent (the notification POST failed) warn in
// both cases. Skipped when nothing was cancelled.
const (
	CancelWorkWarnMs    = 100.0
	CancelWorkFailMs    = 1000.0
	CancelLateShareWarn = 0.1
)

// CancelTool is the tool whose cancelled calls kept running longest on the
// server (by median), for the verdict message.
type CancelTool struct {
	Name  string
	Count int64   // cancelled calls observed by the server
	P50Ms float64 // median work after cancel
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// CancellationVerdict judges a run's cancellations; c.Server is nil when the
// server side was not measured, worst may be nil.
func CancellationVerdict(c *report.Cancellation, worst *CancelTool) report.Verdict {
	const id, signal = report.VerdictCancellation, "mcp_cancellations"
	if c == nil || c.Cancels == 0 {
		return skipped(id, signal, "no call was cancelled (set CANCEL_RATE to test how the server handles cancellation)")
	}
	v := report.Verdict{ID: id, Signal: signal, Status: report.StatusPass}
	warn := func() {
		if v.Status == report.StatusPass {
			v.Status = report.StatusWarn
		}
	}
	cancels := plural(c.Cancels, "cancel", "cancels")
	var facts []string
	if c.SendMs != nil && c.SendMs.Count > 0 {
		facts = append(facts, fmt.Sprintf("a cancel took a median %s to send", FormatMs(c.SendMs.P50)))
	}
	late := plural(c.LateResponses, "late response", "late responses")
	if c.LateResponses > 0 && c.LateAfterMs != nil && c.LateAfterMs.Count > 0 {
		late += fmt.Sprintf(" (a median %s after the cancel)", FormatMs(c.LateAfterMs.P50))
	}
	facts = append(facts, late)
	failed := c.ByOutcome["send_failed"]
	if failed > 0 {
		warn()
		facts = append(facts, plural(failed, "cancel", "cancels")+" could not be sent")
	}

	var msg string
	if s := c.Server; s != nil {
		switch {
		case s.Observed == 0 && s.Cancelled == 0:
			warn()
			msg = fmt.Sprintf("The server registered none of the %s the client sent: it may not handle notifications/cancelled.", cancels)
		case s.WorkAfterCancelP50Ms != nil && *s.WorkAfterCancelP50Ms >= CancelWorkWarnMs:
			if *s.WorkAfterCancelP50Ms >= CancelWorkFailMs {
				v.Status = report.StatusFail
			} else {
				warn()
			}
			what, p50, n := "cancelled calls", *s.WorkAfterCancelP50Ms, s.Observed
			if worst != nil && worst.Name != "" {
				what, p50, n = "`"+worst.Name+"`", worst.P50Ms, worst.Count
			}
			msg = fmt.Sprintf("The server kept running %s for a median %s after %s: cancelled work still uses capacity (%.1f s of handler time in total).",
				what, FormatMs(p50), plural(n, "cancel", "cancels"), s.WorkAfterCancelTotalS)
		default:
			p50 := "-"
			if s.WorkAfterCancelP50Ms != nil {
				p50 = FormatMs(*s.WorkAfterCancelP50Ms)
			}
			msg = fmt.Sprintf("The server stopped cancelled work within a median %s (%s seen by the server of %s sent).",
				p50, plural(s.Cancelled, "cancel", "cancels"), cancels)
		}
	} else {
		share := float64(c.LateResponses) / float64(c.Cancels)
		if share > CancelLateShareWarn {
			warn()
			msg = fmt.Sprintf("%s of %s still got a response afterwards: the server likely kept working on cancelled calls.",
				fmt.Sprint(c.LateResponses), cancels)
		} else {
			msg = fmt.Sprintf("%s sent.", strings.ToUpper(cancels[:1])+cancels[1:])
		}
		msg += " The server side was not measured (use --sampler prometheus with a server exposing mcp_work_after_cancel_seconds)."
	}
	v.Message = msg + " Client: " + strings.Join(facts, ", ") + "."
	return v
}
