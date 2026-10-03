package analysis

import (
	"fmt"
	"sort"
	"strings"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

// Version skew (scenario "version-skew", report/schema/README.md "Verdict ids").
//
// The version-skew scenario runs agent sessions against a load balancer whose
// replicas run different builds (a rolling deploy caught halfway) and
// classifies every failed request (scenarios/lib/skew.js):
//
//   - fast: a typed error (HTTP 4xx/5xx, a JSON-RPC error code, an "unknown
//     tool" result) returned in under FAIL_FAST_MS: clients can handle it;
//   - slow: a typed error slower than that, or one without an HTTP status;
//   - hang: no answer before the client timeout, or a failure that took at
//     least HANG_MS: clients stall for as long as the deploy lasts.
//
// Status: fail when any request hung; warn when requests failed but none hung;
// pass when nothing failed; skipped when the scenario recorded no requests.
// Ordinary tool errors (isError results such as the demo `flaky`) never count.

// Skew failure kinds (the scenario's `kind` tag).
const (
	SkewFast = "fast"
	SkewSlow = "slow"
	SkewHang = "hang"
	// SkewNoAnswer is the replica tag of a request that got no response
	// (timeout, connection error).
	SkewNoAnswer = "no-answer"
)

// SkewInput is what VersionSkewVerdict judges. Failures maps kind to reason to
// count; FailureMedianMs is the median time until failures of each kind
// surfaced; Replicas counts requests by the replica that answered (where
// known); Negotiated counts successful connects by protocol and replica.
type SkewInput struct {
	Requests        int64
	Failures        map[string]map[string]int64
	FailureMedianMs map[string]float64
	Replicas        map[string]int64
	Negotiated      map[string]map[string]int64
}

var skewReasonText = map[string]string{
	"unsupported_protocol": "Unsupported protocol version (-32022)",
	"method_not_found":     "Method not found (-32601)",
	"unknown_tool":         "unknown tool",
	"session_not_found":    "session not found (404)",
	"header_mismatch":      "header mismatch (-32020)",
	"auth":                 "auth error (401/403)",
	"jsonrpc_error":        "other JSON-RPC error",
	"timeout":              "no answer before the client timeout",
	"transport":            "connection error without an HTTP response",
}

// SkewReasonText is the readable form of a failure reason tag.
func SkewReasonText(r string) string {
	if t, ok := skewReasonText[r]; ok {
		return t
	}
	if s, ok := strings.CutPrefix(r, "http_"); ok {
		return "HTTP " + s
	}
	return r
}

type countedKey struct {
	key string
	n   int64
}

// byCount returns m's entries, largest first (ties by key).
func byCount(m map[string]int64) []countedKey {
	out := make([]countedKey, 0, len(m))
	for k, n := range m {
		if n > 0 {
			out = append(out, countedKey{k, n})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].key < out[j].key
	})
	return out
}

func sum(m map[string]int64) int64 {
	var n int64
	for _, v := range m {
		n += v
	}
	return n
}

// reasonList is "340 × Unsupported protocol version (-32022), 22 × HTTP 400".
func reasonList(m map[string]int64) string {
	var parts []string
	for _, c := range byCount(m) {
		parts = append(parts, fmt.Sprintf("%d × %s", c.n, SkewReasonText(c.key)))
	}
	return strings.Join(parts, ", ")
}

// negotiationSummary is "2026-07-28 via skew-new (412), 2025-11-25 via skew-old (130)".
func negotiationSummary(neg map[string]map[string]int64) string {
	flat := map[string]int64{}
	for p, reps := range neg {
		for r, n := range reps {
			flat[p+" via "+r] += n
		}
	}
	var parts []string
	for _, c := range byCount(flat) {
		parts = append(parts, fmt.Sprintf("%s (%d)", c.key, c.n))
	}
	return strings.Join(parts, ", ")
}

func replicaSummary(reps map[string]int64) string {
	total := sum(reps)
	var parts []string
	for _, c := range byCount(reps) {
		name := c.key
		if name == SkewNoAnswer {
			name = "no answer"
		}
		parts = append(parts, fmt.Sprintf("%s %.0f%%", name, 100*float64(c.n)/float64(total)))
	}
	return strings.Join(parts, ", ")
}

// VersionSkewVerdict judges how requests fail when replicas run different builds.
func VersionSkewVerdict(in SkewInput) report.Verdict {
	const id, signal = report.VerdictVersionSkew, "mcp_skew_failures"
	if in.Requests <= 0 {
		return skipped(id, signal, "the version-skew scenario recorded no requests")
	}
	fast, slow, hang := sum(in.Failures[SkewFast]), sum(in.Failures[SkewSlow]), sum(in.Failures[SkewHang])
	failed := fast + slow + hang
	v := report.Verdict{ID: id, Signal: signal, Status: report.StatusPass}

	var context []string
	if neg := negotiationSummary(in.Negotiated); neg != "" {
		context = append(context, "Sessions negotiated "+neg+".")
	}
	if len(in.Replicas) > 0 {
		context = append(context, "Requests by replica: "+replicaSummary(in.Replicas)+".")
	}
	if len(in.Replicas) == 1 && failed == 0 {
		context = append(context, "Only one replica answered: point the run at the load balancer in front of both builds.")
	}
	tail := ""
	if len(context) > 0 {
		tail = " " + strings.Join(context, " ")
	}

	if failed == 0 {
		v.Message = fmt.Sprintf("All %d requests succeeded whichever replica served them: no sign of a version mismatch.%s", in.Requests, tail)
		return v
	}
	pct := 100 * float64(failed) / float64(in.Requests)
	msg := fmt.Sprintf("%d of %d requests (%.1f%%) failed on a replica running a different build", failed, in.Requests, pct)
	var parts []string
	if fast > 0 {
		parts = append(parts, fmt.Sprintf("%d failed fast with typed errors (good: clients can catch them): %s", fast, reasonList(in.Failures[SkewFast])))
	}
	if slow > 0 {
		parts = append(parts, fmt.Sprintf("%d failed slowly or without a typed error (median %s): %s", slow, FormatMs(in.FailureMedianMs[SkewSlow]), reasonList(in.Failures[SkewSlow])))
	}
	if hang > 0 {
		parts = append(parts, fmt.Sprintf("%d hung for a median %s (bad: clients will hang during a rolling deploy): %s", hang, FormatMs(in.FailureMedianMs[SkewHang]), reasonList(in.Failures[SkewHang])))
	}
	msg += "; " + strings.Join(parts, "; ") + "."
	switch {
	case hang > 0:
		v.Status = report.StatusFail
		msg += " Make a replica that can't serve a request reject it with a typed error (e.g. -32022 Unsupported protocol version) instead of leaving it unanswered."
	default:
		v.Status = report.StatusWarn
		msg += " Mismatches surface as errors clients can handle; pin sessions to one build (or drain old replicas) to avoid them."
	}
	v.Message = msg + tail
	return v
}
