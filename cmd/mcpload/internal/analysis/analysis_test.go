package analysis

import (
	"math"
	"path/filepath"
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
	r.Verdicts = append(Verdicts(r, DefaultConfig()), ThresholdVerdict(r.Thresholds), SessionNotFoundVerdict(0, 10))
	if err := r.Check(); err != nil {
		t.Fatal(err)
	}
}

func TestDriftWarn(t *testing.T) {
	r := load(t, "healthy.json")
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
	if SessionNotFoundVerdict(3, 1000).Status != report.StatusFail || SessionNotFoundVerdict(0, 1000).Status != report.StatusPass {
		t.Error("session_not_found")
	}
}
