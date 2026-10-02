package analysis

import (
	"strings"
	"testing"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

func TestIsolationPooledServerFails(t *testing.T) {
	solo := map[string]PhaseP95{"fast": {Calls: 900, P95: 3.1}, "search": {Calls: 1500, P95: 4.2}}
	mixed := map[string]PhaseP95{"fast": {Calls: 300, P95: 298.4}, "search": {Calls: 500, P95: 312}, "slow": {Calls: 100, P95: 610}}
	v := IsolationVerdict(solo, mixed)
	if v.Status != report.StatusFail {
		t.Fatalf("status %s, want fail: %s", v.Status, v.Message)
	}
	for _, want := range []string{"2 of 2 tools wait behind slow", "search p95 4.2 ms alone → 312.0 ms mixed"} {
		if !strings.Contains(v.Message, want) {
			t.Errorf("message %q lacks %q", v.Message, want)
		}
	}
}

func TestIsolationHealthyServerPasses(t *testing.T) {
	solo := map[string]PhaseP95{"fast": {Calls: 900, P95: 3.1}, "search": {Calls: 1500, P95: 4.2}}
	mixed := map[string]PhaseP95{"fast": {Calls: 300, P95: 3.4}, "search": {Calls: 500, P95: 4.9}, "slow": {Calls: 100, P95: 305}}
	v := IsolationVerdict(solo, mixed)
	if v.Status != report.StatusPass {
		t.Fatalf("status %s, want pass: %s", v.Status, v.Message)
	}
	if !strings.Contains(v.Message, "with slow running alongside") {
		t.Errorf("message %q does not name the slow tool", v.Message)
	}
}

// A fast tool tripling from 2 ms to 6 ms is noise, not a shared bottleneck.
func TestIsolationSmallAbsoluteRiseDoesNotWarn(t *testing.T) {
	v := IsolationVerdict(map[string]PhaseP95{"fast": {Calls: 500, P95: 2}}, map[string]PhaseP95{"fast": {Calls: 500, P95: 6}, "slow": {Calls: 50, P95: 300}})
	if v.Status != report.StatusPass {
		t.Fatalf("status %s, want pass: %s", v.Status, v.Message)
	}
}

func TestIsolationModerateRiseWarns(t *testing.T) {
	v := IsolationVerdict(map[string]PhaseP95{"search": {Calls: 500, P95: 60}}, map[string]PhaseP95{"search": {Calls: 500, P95: 100}, "slow": {Calls: 50, P95: 300}})
	if v.Status != report.StatusWarn {
		t.Fatalf("status %s, want warn: %s", v.Status, v.Message)
	}
}

func TestIsolationSkipped(t *testing.T) {
	few := map[string]PhaseP95{"fast": {Calls: 5, P95: 3}}
	for name, c := range map[string][2]map[string]PhaseP95{
		"no solo phase":  {nil, {"fast": {Calls: 500, P95: 3}}},
		"no mixed phase": {{"fast": {Calls: 500, P95: 3}}, nil},
		"too few calls":  {few, {"fast": {Calls: 5, P95: 3}, "slow": {Calls: 50, P95: 300}}},
		"no slow tool":   {{"fast": {Calls: 500, P95: 3}}, {"fast": {Calls: 500, P95: 30}}},
	} {
		if v := IsolationVerdict(c[0], c[1]); v.Status != report.StatusSkipped {
			t.Errorf("%s: status %s, want skipped", name, v.Status)
		}
	}
}
