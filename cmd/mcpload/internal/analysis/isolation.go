package analysis

import (
	"fmt"
	"sort"
	"strings"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

// Tool isolation (scenario "isolation", report/schema/README.md "Verdict ids").
//
// The isolation scenario runs the same agent sessions in two k6 scenarios at
// the same concurrency: "solo" (the tool mix without the slow tools) and
// "mixed" (the full mix). A tool that is fast on its own but waits behind the
// slow tools when they run alongside points at a shared bottleneck (connection
// or worker pool, a blocked event loop).
//
//   - Compared: every tool with at least IsolationMinCalls successful calls in
//     both phases. Slow tools only appear in "mixed" and are named in the message.
//   - fail when, for any tool, mixed p95 >= IsolationFailRatio x solo p95 AND
//     the rise is at least IsolationFailMinMs; warn at IsolationWarnRatio and
//     IsolationWarnMinMs. The absolute floors stop tools of a few ms from
//     failing on noise.
//   - skipped when either phase is missing, the mixed phase called no extra
//     (slow) tool, or no tool is comparable.
const (
	IsolationFailRatio = 2.0
	IsolationFailMinMs = 50.0
	IsolationWarnRatio = 1.5
	IsolationWarnMinMs = 25.0
	IsolationMinCalls  = 30

	// IsolationSoloScenario and IsolationMixedScenario are the k6 scenario
	// names scenarios/isolation.js uses.
	IsolationSoloScenario  = "solo"
	IsolationMixedScenario = "mixed"
)

// PhaseP95 is one tool's successful-call count and p95 (ms) within a phase.
type PhaseP95 struct {
	Calls int
	P95   float64
}

type isoCmp struct {
	name        string
	solo, mixed float64
	ratio, rise float64
	status      string
}

// IsolationVerdict compares per-tool p95 between the solo and mixed phases.
func IsolationVerdict(solo, mixed map[string]PhaseP95) report.Verdict {
	const id, signal = report.VerdictToolIsolation, "tools.p95Ms (solo vs mixed)"
	if len(solo) == 0 || len(mixed) == 0 {
		return skipped(id, signal, "needs tool calls in both the solo and the mixed phase (scenario isolation)")
	}
	var slow []string
	for n := range mixed {
		if _, ok := solo[n]; !ok {
			slow = append(slow, n)
		}
	}
	if len(slow) == 0 {
		return skipped(id, signal, "the mixed phase called no tool the solo phase didn't, so there was nothing to compare against (check SLOW_TOOLS)")
	}
	sort.Strings(slow)
	slowDesc := strings.Join(slow, ", ")

	var cmps []isoCmp
	for n, s := range solo {
		m, ok := mixed[n]
		if !ok || s.Calls < IsolationMinCalls || m.Calls < IsolationMinCalls {
			continue
		}
		c := isoCmp{name: n, solo: s.P95, mixed: m.P95, rise: m.P95 - s.P95, status: report.StatusPass}
		if s.P95 > 0 {
			c.ratio = m.P95 / s.P95
		}
		switch {
		case c.ratio >= IsolationFailRatio && c.rise >= IsolationFailMinMs:
			c.status = report.StatusFail
		case c.ratio >= IsolationWarnRatio && c.rise >= IsolationWarnMinMs:
			c.status = report.StatusWarn
		}
		cmps = append(cmps, c)
	}
	if len(cmps) == 0 {
		return skipped(id, signal, fmt.Sprintf("no tool had at least %d successful calls in both phases", IsolationMinCalls))
	}
	rank := map[string]int{report.StatusPass: 0, report.StatusWarn: 1, report.StatusFail: 2}
	sort.SliceStable(cmps, func(i, j int) bool {
		if rank[cmps[i].status] != rank[cmps[j].status] {
			return rank[cmps[i].status] > rank[cmps[j].status]
		}
		if cmps[i].rise != cmps[j].rise {
			return cmps[i].rise > cmps[j].rise
		}
		return cmps[i].name < cmps[j].name
	})
	desc := func(c isoCmp) string {
		return fmt.Sprintf("%s p95 %.1f ms alone → %.1f ms mixed (×%.2f)", c.name, c.solo, c.mixed, c.ratio)
	}
	worst := cmps[0]
	v := report.Verdict{ID: id, Signal: signal, Status: worst.status, Baseline: report.F(round(worst.solo, 3))}
	if worst.status == report.StatusPass {
		v.Message = fmt.Sprintf("%d tools kept their latency with %s running alongside; largest change: %s.", len(cmps), slowDesc, desc(worst))
		return v
	}
	var bad []string
	for _, c := range cmps {
		if c.status != report.StatusPass {
			bad = append(bad, desc(c))
		}
	}
	verb := "slow down"
	if worst.status == report.StatusFail {
		verb = "wait behind"
	}
	v.Message = fmt.Sprintf("%d of %d tools %s %s: %s. Typical causes: a connection or worker pool shared by all tools, or slow work blocking the event loop.",
		len(bad), len(cmps), verb, slowDesc, strings.Join(bad, "; "))
	return v
}
