package analysis

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

// synth builds a report with phases warm-up [0,warm), load [warm,loadEnd),
// cool-down [loadEnd,coolEnd] and 30 s buckets. Series are filled by the caller.
func synth(warm, loadEnd, coolEnd float64) *report.Report {
	r := &report.Report{Phases: report.Phases{WarmupEndS: warm, LoadEndS: loadEnd, CooldownEndS: coolEnd}}
	r.Series.IntervalS = 30
	r.Series.Server.Sampler = report.SamplerPrometheus
	for t := 0.0; t <= coolEnd; t += 30 {
		r.Series.T = append(r.Series.T, t)
	}
	n := len(r.Series.T)
	r.Series.Client.P95Ms = make([]*float64, n)
	r.Series.Client.ErrorRate = make([]*float64, n)
	r.Series.Client.RPS = make([]*float64, n)
	return r
}

func gen(r *report.Report, fn func(i int, t float64) float64) []*float64 {
	out := make([]*float64, len(r.Series.T))
	for i, t := range r.Series.T {
		out[i] = report.F(fn(i, t))
	}
	return out
}

func noise(seed uint64) func() float64 {
	rnd := rand.New(rand.NewPCG(seed, 7))
	return rnd.NormFloat64
}

func phaseOf(r *report.Report, t float64) string {
	switch {
	case t < r.Phases.WarmupEndS:
		return "warm"
	case t < r.Phases.LoadEndS:
		return "load"
	}
	return "cool"
}

func mustStatus(t *testing.T, v report.Verdict, want string) {
	t.Helper()
	t.Logf("%s %s [%s]: %s", v.ID, v.Status, v.Signal, v.Message)
	if v.Status != want {
		t.Fatalf("%s = %s, want %s: %s", v.ID, v.Status, want, v.Message)
	}
}

// Short runs (no cool-down, or < 2 min of load) cannot tell startup growth
// from a leak: all leak verdicts are skipped.
func TestShortRunSkipped(t *testing.T) {
	cases := map[string]*report.Report{
		"15s agent-session, no cool-down": synth(0, 15, 15),
		"90s load with cool-down":         synth(0, 90, 150),
		"no cool-down":                    synth(60, 1200, 1200),
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			// Steep startup growth that would fail any regression starting at t=0.
			r.Series.Server.RSSBytes = gen(r, func(_ int, x float64) float64 { return 50*mib + 4*mib*x })
			r.Series.Server.ActiveSessions = gen(r, func(_ int, x float64) float64 { return x / 2 })
			r.Series.Server.OpenFDs = gen(r, func(_ int, x float64) float64 { return 10 + x })
			m := byID(Verdicts(r, DefaultConfig()))
			for _, id := range []string{report.VerdictMemoryLeak, report.VerdictSessionLeak, report.VerdictFDLeak} {
				mustStatus(t, m[id], report.StatusSkipped)
				if !strings.Contains(m[id].Message, "needs a soak run with cool-down") {
					t.Errorf("%s message: %s", id, m[id].Message)
				}
			}
		})
	}
}

// Startup growth in the first minute of a run without warm-up is not a leak.
func TestStartupGrowthNotALeak(t *testing.T) {
	r := synth(0, 720, 900)
	nz := noise(1)
	r.Series.Server.RSSBytes = gen(r, func(_ int, x float64) float64 {
		return 100*mib + 30*mib*math.Min(x/45, 1) + 0.5*mib*nz()
	})
	r.Series.Server.HeapBytes = gen(r, func(_ int, x float64) float64 {
		return 40*mib + 20*mib*math.Min(x/45, 1) + 0.3*mib*nz()
	})
	r.Series.Server.ActiveSessions = gen(r, func(_ int, x float64) float64 {
		if phaseOf(r, x) == "cool" && x >= 750 {
			return 0
		}
		return math.Min(x/3, 10)
	})
	r.Series.Server.OpenFDs = gen(r, func(_ int, x float64) float64 {
		if phaseOf(r, x) == "cool" {
			return 21
		}
		return 20 + math.Min(x/2, 20)
	})
	m := byID(Verdicts(r, DefaultConfig()))
	for _, id := range []string{report.VerdictMemoryLeak, report.VerdictSessionLeak, report.VerdictFDLeak} {
		mustStatus(t, m[id], report.StatusPass)
	}
}

// A perfectly linear rise above the slope limit whose total is below the growth floor passes.
func TestGrowthFloor(t *testing.T) {
	r := synth(60, 240, 420) // 3 min of load
	r.Series.Server.RSSBytes = gen(r, func(_ int, x float64) float64 {
		if x >= 240 {
			return 100 * mib
		}
		return 100*mib + 1.5*mib*math.Max(x-60, 0)/60 // 1.5 MiB/min, 4.5 MiB total
	})
	v := Verdicts(r, DefaultConfig())[0]
	mustStatus(t, v, report.StatusPass)
	if !strings.Contains(v.Message, "growth floor") || !strings.Contains(v.Message, "only fast leaks") {
		t.Errorf("message: %s", v.Message)
	}
	// The same slope over 10 min (15 MiB) is a leak.
	r = synth(60, 660, 840)
	r.Series.Server.RSSBytes = gen(r, func(_ int, x float64) float64 {
		return 100*mib + 1.5*mib*math.Max(math.Min(x, 660)-60, 0)/60
	})
	mustStatus(t, Verdicts(r, DefaultConfig())[0], report.StatusFail)
}

// sawtooth is a GC-like pattern: rises over 4 buckets, then drops.
func sawtooth(i int, amp float64) float64 { return float64(i%4) / 3 * amp }

// Healthy servers with a large GC sawtooth and noise never fail memory_leak
// (lower-envelope fit must not create false positives).
func TestSawtoothHealthyPasses(t *testing.T) {
	cfg := DefaultConfig()
	for seed := uint64(0); seed < 200; seed++ {
		r := synth(240, 240+[]float64{300, 600, 1200, 1920}[seed%4], 240+[]float64{300, 600, 1200, 1920}[seed%4]+240)
		nz := noise(seed)
		off := int(seed % 4)
		r.Series.Server.RSSBytes = gen(r, func(i int, x float64) float64 {
			return 150*mib + sawtooth(i+off, 20*mib) + 2.5*mib*nz()
		})
		r.Series.Server.HeapBytes = gen(r, func(i int, x float64) float64 {
			return 80*mib + sawtooth(i+off, 30*mib) + 2*mib*nz()
		})
		v := Verdicts(r, cfg)[0]
		if v.Status != report.StatusPass {
			t.Fatalf("seed %d: %s: %s", seed, v.Status, v.Message)
		}
	}
}

// A slow steady leak (3 MiB/min over 10 min) under a GC sawtooth fails, with
// or without cool-down recovery.
func TestSlowSteadyLeakFails(t *testing.T) {
	for _, recovers := range []bool{false, true} {
		r := synth(120, 720, 960)
		nz := noise(42)
		r.Series.Server.RSSBytes = gen(r, func(i int, x float64) float64 {
			mins := math.Max(math.Min(x, 720)-120, 0) / 60
			if recovers && x >= 720 {
				mins = 0
			}
			return 150*mib + 3*mib*mins + sawtooth(i, 12*mib) + 1.5*mib*nz()
		})
		v := Verdicts(r, DefaultConfig())[0]
		mustStatus(t, v, report.StatusFail)
		if s := *v.SlopePerMin / mib; s < 2.5 || s > 3.5 {
			t.Errorf("slope %.2f MiB/min", s)
		}
	}
}

// 10 concurrent sessions under load; a slow leak leaves 12 behind after
// cool-down. Comparing to the under-load baseline (10) would hide it.
func TestSessionsRetainedFails(t *testing.T) {
	r := synth(240, 2160, 2400)
	r.Series.Server.ActiveSessions = gen(r, func(_ int, x float64) float64 {
		stuck := math.Round(0.4 * math.Max(math.Min(x, 2160)-240, 0) / 60 * 12 / 12.8)
		switch phaseOf(r, x) {
		case "warm":
			return math.Round(10 * x / 240)
		case "load":
			return 10 + stuck
		}
		return stuck
	})
	v := Verdicts(r, DefaultConfig())[1]
	mustStatus(t, v, report.StatusFail)
	if v.CooldownRecovered == nil || *v.CooldownRecovered || !strings.Contains(v.Message, "12 still open vs 0 idle") {
		t.Errorf("message: %s", v.Message)
	}
	// Same shape but all sessions closed in cool-down: pass (slope 0.4/min < 0.5 limit).
	r.Series.Server.ActiveSessions = gen(r, func(_ int, x float64) float64 {
		if phaseOf(r, x) == "cool" {
			return 1
		}
		return 10
	})
	mustStatus(t, Verdicts(r, DefaultConfig())[1], report.StatusPass)
}

// FDs: idle level is the warm-up minimum; 6 retained fails, 3 passes.
func TestFDRetained(t *testing.T) {
	for _, c := range []struct {
		retained float64
		want     string
	}{{3, report.StatusPass}, {6, report.StatusFail}} {
		r := synth(240, 1200, 1440)
		r.Series.Server.OpenFDs = gen(r, func(_ int, x float64) float64 {
			switch phaseOf(r, x) {
			case "warm":
				return 20 + 30*x/240
			case "load":
				return 50
			}
			return 20 + c.retained
		})
		mustStatus(t, Verdicts(r, DefaultConfig())[2], c.want)
	}
}

// Heap grows while RSS stays flat (runtime reserved the memory up front):
// memory_leak fails on the heap signal.
func TestHeapLeakDetected(t *testing.T) {
	r := synth(240, 1440, 1680)
	nz := noise(3)
	r.Series.Server.RSSBytes = gen(r, func(i int, _ float64) float64 { return 300*mib + sawtooth(i, 4*mib) + mib*nz() })
	r.Series.Server.HeapBytes = gen(r, func(i int, x float64) float64 {
		return 60*mib + 2*mib*math.Max(math.Min(x, 1440)-240, 0)/60 + sawtooth(i, 8*mib) + mib*nz()
	})
	v := Verdicts(r, DefaultConfig())[0]
	mustStatus(t, v, report.StatusFail)
	if v.Signal != "server.heapBytes" || !strings.HasPrefix(v.Message, "Heap grew") || !strings.Contains(v.Message, "RSS flat") {
		t.Errorf("signal %s message %s", v.Signal, v.Message)
	}
	// Without the heap series the same run passes on RSS alone.
	r.Series.Server.HeapBytes = nil
	mustStatus(t, Verdicts(r, DefaultConfig())[0], report.StatusPass)
}

// A steep slope with low R² is "no consistent trend", not "flat".
func TestLowR2Message(t *testing.T) {
	r := synth(240, 1440, 1680)
	nz := noise(9)
	r.Series.Server.OpenFDs = gen(r, func(_ int, x float64) float64 {
		if phaseOf(r, x) == "cool" {
			return 30
		}
		return 30 + 1.5*math.Max(x-240, 0)/60 + 40*nz()
	})
	v := Verdicts(r, DefaultConfig())[2]
	t.Log(v.Message)
	if *v.R2 >= 0.7 || *v.SlopePerMin <= 1 {
		t.Skipf("noise draw gave slope %v r2 %v", *v.SlopePerMin, *v.R2)
	}
	if !strings.Contains(v.Message, "no consistent trend under constant load (slope") || !strings.Contains(v.Message, "below 0.7)") || strings.Contains(v.Message, "flat") {
		t.Errorf("message: %s", v.Message)
	}
}

// One tool regressing 20× must warn even when the mixed p95 is flapping.
func TestPerToolDriftWarns(t *testing.T) {
	r := synth(240, 2160, 2400)
	nz := noise(5)
	r.Series.Client.P95Ms = gen(r, func(i int, x float64) float64 {
		if i%2 == 0 {
			return 900 + 50*nz()
		}
		return 300 + 50*nz()
	})
	r.Series.Tools = map[string]report.ToolSeries{
		"search": {P95Ms: gen(r, func(_ int, _ float64) float64 { return 400 + 10*nz() })},
		"fast":   {P95Ms: gen(r, func(_ int, x float64) float64 { return 10 + 190*math.Max(x-240, 0)/1920 + nz() })},
		"slow":   {P95Ms: gen(r, func(_ int, _ float64) float64 { return 900 + 30*nz() })},
	}
	v := byID(Verdicts(r, DefaultConfig()))[report.VerdictLatencyDrift]
	mustStatus(t, v, report.StatusWarn)
	if v.Signal != "tools.fast.p95Ms" || !strings.Contains(v.Message, "1 of 3 tools: fast") {
		t.Errorf("signal %s message %s", v.Signal, v.Message)
	}
	// Without per-tool series the flapping mixed p95 hides it.
	r.Series.Tools = nil
	mustStatus(t, byID(Verdicts(r, DefaultConfig()))[report.VerdictLatencyDrift], report.StatusPass)
}

// A fast tool going from 6 ms to 10 ms is a big percentage but not a real
// regression: the absolute floor must keep it from warning.
func TestFastToolSmallRiseDoesNotWarn(t *testing.T) {
	r := synth(60, 660, 720)
	r.Series.Tools = map[string]report.ToolSeries{
		"fast": {P95Ms: gen(r, func(_ int, x float64) float64 { return 6 + 4*math.Max(x-60, 0)/600 })},
	}
	mustStatus(t, byID(Verdicts(r, DefaultConfig()))[report.VerdictLatencyDrift], report.StatusPass)
}

// Drift needs at least DriftMinLoadS of load; a 20 s run is skipped.
func TestDriftSkippedOnShortRun(t *testing.T) {
	r := synth(0, 20, 20)
	r.Series.IntervalS = 5
	r.Series.T = []float64{0, 5, 10, 15}
	r.Series.Client.P95Ms = gen(r, func(i int, _ float64) float64 { return 10 + 10*float64(i) })
	r.Series.Client.ErrorRate = gen(r, func(i int, _ float64) float64 { return 0.01 * float64(i) })
	m := byID(Verdicts(r, DefaultConfig()))
	mustStatus(t, m[report.VerdictLatencyDrift], report.StatusSkipped)
	mustStatus(t, m[report.VerdictErrorDrift], report.StatusSkipped)
}

func TestGeneratorVerdict(t *testing.T) {
	gen := func(iter, dropped *int64, cpu *float64) report.Verdict {
		r := &report.Report{}
		r.Summary.Iterations, r.Summary.DroppedIterations = iter, dropped
		if cpu != nil {
			r.Run.Generator = &report.Generator{Cores: 8, CPUAvgPct: report.F(*cpu / 2), CPUMaxPct: cpu}
		}
		return GeneratorVerdict(r)
	}
	i := report.I64
	cases := []struct {
		name    string
		v       report.Verdict
		status  string
		signal  string
		message string
	}{
		{"missing", gen(nil, nil, nil), report.StatusSkipped, "", "Skipped"},
		{"no cpu value", gen(nil, nil, nil), report.StatusSkipped, "", "Skipped"},
		{"ok", gen(i(1000), i(5), report.F(40)), report.StatusPass, "summary.droppedIterations", "1000 of 1005"},
		{"1.5% dropped", gen(i(985), i(15), nil), report.StatusWarn, "summary.droppedIterations", "1.5% of planned iterations were dropped"},
		{"20% dropped", gen(i(800), i(200), report.F(30)), report.StatusFail, "summary.droppedIterations", "only 80.0% of planned iterations ran"},
		{"cpu hot", gen(i(1000), i(0), report.F(95)), report.StatusWarn, "run.generator.cpuMaxPct", "k6 used up to ~95% of CPU of 8 cores, ~48% on average"},
		{"cpu 88 ok", gen(i(1000), i(0), report.F(88)), report.StatusPass, "summary.droppedIterations", "up to ~88%"},
		{"cpu only ok", gen(nil, nil, report.F(50)), report.StatusPass, "run.generator.cpuMaxPct", "up to ~50%"},
	}
	for _, c := range cases {
		t.Logf("%s: %s %s %s", c.name, c.v.Status, c.v.Signal, c.v.Message)
		if c.v.Status != c.status || (c.signal != "" && c.v.Signal != c.signal) || !strings.Contains(c.v.Message, c.message) {
			t.Errorf("%s: got %s %s %q", c.name, c.v.Status, c.v.Signal, c.v.Message)
		}
		if c.v.ID != report.VerdictGenerator {
			t.Errorf("id %s", c.v.ID)
		}
	}
	// A generator fail fails the run.
	r := &report.Report{Verdicts: []report.Verdict{gen(i(800), i(200), nil)}}
	if report.Passed(r) {
		t.Error("generator fail should fail the run")
	}
}
