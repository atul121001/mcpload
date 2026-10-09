package analysis

import (
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

func load(t *testing.T, name string) *report.Report {
	t.Helper()
	r, err := report.ReadJSON(filepath.Join("../../../../report/examples", name))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func byID(vs []report.Verdict) map[string]report.Verdict {
	m := map[string]report.Verdict{}
	for _, v := range vs {
		m[v.ID] = v
	}
	return m
}

func TestLinRegExact(t *testing.T) {
	x := []float64{0, 1, 2, 3, math.NaN(), 5}
	y := []float64{1, 3, 5, 7, 100, 11} // y = 2x+1, NaN pair skipped
	s, i, r2 := LinReg(x, y)
	if math.Abs(s-2) > 1e-12 || math.Abs(i-1) > 1e-12 || math.Abs(r2-1) > 1e-12 {
		t.Fatalf("got %v %v %v", s, i, r2)
	}
	s, i, r2 = LinReg([]float64{1, 2, 3}, []float64{4, 4, 4})
	if s != 0 || i != 4 || r2 != 0 {
		t.Fatalf("flat: %v %v %v", s, i, r2)
	}
	s, _, _ = LinReg([]float64{1}, []float64{1})
	if !math.IsNaN(s) {
		t.Fatal("expected NaN for n<2")
	}
	// Known noisy case: x=1..5, y=[2,4,5,4,5] -> slope 0.6, intercept 2.2, r2 0.6
	s, i, r2 = LinReg([]float64{1, 2, 3, 4, 5}, []float64{2, 4, 5, 4, 5})
	if math.Abs(s-0.6) > 1e-12 || math.Abs(i-2.2) > 1e-12 || math.Abs(r2-0.6) > 1e-12 {
		t.Fatalf("noisy: %v %v %v", s, i, r2)
	}
}

func TestVerdictsHealthy(t *testing.T) {
	r := load(t, "healthy.json")
	vs := Verdicts(r, DefaultConfig())
	if len(vs) != 5 {
		t.Fatalf("want 5 verdicts, got %d", len(vs))
	}
	for _, v := range vs {
		t.Logf("%s %s: %s", v.ID, v.Status, v.Message)
		if v.Status != report.StatusPass {
			t.Errorf("%s = %s, want pass", v.ID, v.Status)
		}
	}
}

func TestVerdictsLeaky(t *testing.T) {
	r := load(t, "leaky.json")
	m := byID(Verdicts(r, DefaultConfig()))
	for _, v := range m {
		t.Logf("%s %s: %s", v.ID, v.Status, v.Message)
	}
	ml := m["memory_leak"]
	if ml.Status != report.StatusFail {
		t.Fatalf("memory_leak = %s", ml.Status)
	}
	mb := *ml.SlopePerMin / (1 << 20)
	if mb < 2.7 || mb > 3.3 {
		t.Errorf("slope %.2f MiB/min, want ~3", mb)
	}
	if *ml.R2 < 0.9 {
		t.Errorf("r2 %v", *ml.R2)
	}
	if ml.CooldownRecovered == nil || *ml.CooldownRecovered {
		t.Error("cooldownRecovered should be false")
	}
	if m["session_leak"].Status != report.StatusFail {
		t.Error("session_leak should fail")
	}
	if m["fd_leak"].Status != report.StatusPass {
		t.Error("fd_leak should pass")
	}
}

func TestVerdictsSkipped(t *testing.T) {
	r := load(t, "healthy.json")
	r.Series.Server = report.ServerSeries{Sampler: report.SamplerNone, RSSBytes: []*float64{}}
	m := byID(Verdicts(r, DefaultConfig()))
	for _, id := range []string{"memory_leak", "session_leak", "fd_leak"} {
		if m[id].Status != report.StatusSkipped {
			t.Errorf("%s = %s, want skipped", id, m[id].Status)
		}
	}
	if m["latency_drift"].Status != report.StatusPass {
		t.Error("latency_drift should still be computed")
	}
	// docker: only RSS
	r = load(t, "healthy.json")
	r.Series.Server.Sampler = report.SamplerDocker
	r.Series.Server.OpenFDs, r.Series.Server.ActiveSessions, r.Series.Server.HeapBytes = nil, nil, nil
	m = byID(Verdicts(r, DefaultConfig()))
	if m["memory_leak"].Status != report.StatusPass || m["fd_leak"].Status != report.StatusSkipped {
		t.Errorf("docker: %s %s", m["memory_leak"].Status, m["fd_leak"].Status)
	}
	// The resulting report must still pass Check.
	r.Verdicts = append(Verdicts(r, DefaultConfig()), ThresholdVerdict(r.Thresholds), SessionNotFoundVerdict(r.Run.Protocol, 0, 10))
	if err := r.Check(); err != nil {
		t.Fatal(err)
	}
}

// A small residue after cool-down (runtimes keep some memory) must not fail
// memory_leak; a residue above clamp(limit * load minutes, 5 MiB, 10 MiB) must.
func TestCooldownResidue(t *testing.T) {
	const mib = 1024 * 1024
	set := func(residueMiB float64) report.Verdict {
		r := load(t, "healthy.json")
		for i, ti := range r.Series.T {
			v := 100.0 * mib
			if ti >= r.Phases.LoadEndS {
				v += residueMiB * mib
			}
			r.Series.Server.RSSBytes[i] = report.F(v)
		}
		return byID(Verdicts(r, DefaultConfig()))["memory_leak"]
	}
	small := set(1.8) // load window is 32 min; allowed is capped at 10 MiB
	if small.Status != report.StatusPass || small.CooldownRecovered == nil || *small.CooldownRecovered {
		t.Fatalf("small residue: status %s, recovered %v; want pass with cooldownRecovered=false (%s)", small.Status, small.CooldownRecovered, small.Message)
	}
	large := set(12)
	if large.Status != report.StatusFail {
		t.Fatalf("large residue: status %s, want fail (%s)", large.Status, large.Message)
	}
}

func TestDriftWarn(t *testing.T) {
	r := load(t, "healthy.json")
	r.Series.Tools = nil // judge the client p95 fallback
	for i, ti := range r.Series.T {
		if v := r.Series.Client.P95Ms[i]; v != nil {
			*v = 400 + 10*ti/60 // +10 ms/min; 32 min -> +320 ms = +80%
		}
		if v := r.Series.Client.ErrorRate[i]; v != nil {
			*v = 0.001 * ti / 60 // +0.032 over load window
		}
	}
	m := byID(Verdicts(r, DefaultConfig()))
	if m["latency_drift"].Status != report.StatusWarn || m["error_drift"].Status != report.StatusWarn {
		t.Fatalf("got %s / %s", m["latency_drift"].Status, m["error_drift"].Status)
	}
}

func TestHelpers(t *testing.T) {
	r := load(t, "leaky.json")
	if v := ThresholdVerdict(r.Thresholds); v.Status != report.StatusFail {
		t.Error("leaky thresholds should fail")
	}
	if v := ThresholdVerdict(load(t, "healthy.json").Thresholds); v.Status != report.StatusPass {
		t.Error("healthy thresholds should pass")
	}
	if SessionNotFoundVerdict("2025-11-25", 3, 1000).Status != report.StatusFail || SessionNotFoundVerdict("2025-11-25", 0, 1000).Status != report.StatusPass {
		t.Error("session_not_found")
	}
}

func TestIsStateless(t *testing.T) {
	for v, want := range map[string]bool{
		"2026-07-28": true, "2027-01-01": true,
		"2025-11-25": false, "2025-06-18": false,
		"": false, "auto": false, "unknown": false, "2026-13-99": false,
	} {
		if got := IsStateless(v); got != want {
			t.Errorf("IsStateless(%q) = %v, want %v", v, got, want)
		}
	}
}

// Session verdicts are judged on stateful protocols and skipped on the
// stateless one (a pass there would be vacuous: there are no sessions).
func TestSessionVerdictsByProtocol(t *testing.T) {
	const skipMsg = "Skipped: the stateless protocol (2026-07-28) has no sessions."
	alive := SessionEnd{LifetimeS: 600}
	dead := SessionEnd{LifetimeS: 302, Died: true, Cause: "session_not_found"}
	healthy := LongSessions([]SessionEnd{alive, alive}, 0, nil, nil)
	dying := LongSessions([]SessionEnd{alive, dead}, 1, nil, nil)
	cases := []struct {
		name, protocol, status, msg string
		v                           func(protocol string) report.Verdict
	}{
		{"snf stateful clean", "2025-11-25", report.StatusPass, "No 404 session-not-found responses.", func(p string) report.Verdict { return SessionNotFoundVerdict(p, 0, 1000) }},
		{"snf stateful 404s", "2025-11-25", report.StatusFail, "3 of 1000 requests", func(p string) report.Verdict { return SessionNotFoundVerdict(p, 3, 1000) }},
		{"snf stateless clean", "2026-07-28", report.StatusSkipped, skipMsg, func(p string) report.Verdict { return SessionNotFoundVerdict(p, 0, 1000) }},
		// A 404 that did happen is never hidden, whatever the protocol.
		{"snf stateless 404s", "2026-07-28", report.StatusFail, "3 of 1000 requests", func(p string) report.Verdict { return SessionNotFoundVerdict(p, 3, 1000) }},
		{"snf unknown protocol", "unknown", report.StatusPass, "No 404", func(p string) report.Verdict { return SessionNotFoundVerdict(p, 0, 1000) }},
		{"survival stateful healthy", "2025-11-25", report.StatusPass, "All 2 sessions stayed open", func(p string) report.Verdict { return SessionSurvivalVerdict(p, healthy) }},
		{"survival stateful deaths", "2025-11-25", report.StatusFail, "1 of 2 sessions died", func(p string) report.Verdict { return SessionSurvivalVerdict(p, dying) }},
		{"survival stateful none", "2025-11-25", report.StatusSkipped, "no long-lived session", func(p string) report.Verdict { return SessionSurvivalVerdict(p, nil) }},
		{"survival stateless", "2026-07-28", report.StatusSkipped, skipMsg, func(p string) report.Verdict { return SessionSurvivalVerdict(p, healthy) }},
		{"survival stateless none", "2026-07-28", report.StatusSkipped, skipMsg, func(p string) report.Verdict { return SessionSurvivalVerdict(p, nil) }},
	}
	for _, c := range cases {
		v := c.v(c.protocol)
		if v.Status != c.status || !strings.Contains(v.Message, c.msg) {
			t.Errorf("%s: got %s %q, want %s containing %q", c.name, v.Status, v.Message, c.status, c.msg)
		}
	}
	// A skipped session verdict leaves the overall result a pass.
	r := load(t, "healthy.json")
	r.Run.Protocol = "2026-07-28"
	r.Verdicts = append(Verdicts(r, DefaultConfig()), SessionNotFoundVerdict(r.Run.Protocol, 0, 10), SessionSurvivalVerdict(r.Run.Protocol, nil))
	if err := r.Check(); err != nil {
		t.Fatal(err)
	}
	if !report.Passed(r) || report.HasWarnings(r) {
		t.Error("skipped session verdicts must not fail or warn the run")
	}
}
