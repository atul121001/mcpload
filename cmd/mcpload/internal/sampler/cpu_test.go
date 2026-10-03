package sampler

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestCPUPercent(t *testing.T) {
	// 2 s of CPU in 1 s of wall time on 8 cores = 25% of the machine.
	if p := CPUPercent(2*time.Second, time.Second, 8); !approx(p, 25) {
		t.Errorf("got %v, want 25", p)
	}
	// One fully busy core of 4.
	if p := CPUPercent(10*time.Second, 10*time.Second, 4); !approx(p, 25) {
		t.Errorf("got %v, want 25", p)
	}
	// Clamped to [0,100] (counter granularity can overshoot).
	if p := CPUPercent(9*time.Second, time.Second, 8); p != 100 {
		t.Errorf("got %v, want 100", p)
	}
	if p := CPUPercent(-time.Second, time.Second, 8); p != 0 {
		t.Errorf("got %v, want 0", p)
	}
	if !math.IsNaN(CPUPercent(time.Second, 0, 8)) || !math.IsNaN(CPUPercent(time.Second, time.Second, 0)) {
		t.Error("empty window must be NaN")
	}
}

func TestCPUMonitorMath(t *testing.T) {
	m := &CPUMonitor{cores: 4, interval: 10 * time.Second}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Readings: baseline 1 s CPU at t0+1s (startup), then +10 s CPU per 10 s
	// (1 core = 25%), then +30 s per 10 s (75%).
	m.add(t0.Add(1*time.Second), 1*time.Second)
	m.add(t0.Add(11*time.Second), 11*time.Second)
	m.add(t0.Add(21*time.Second), 41*time.Second)
	// Final window of 2 s is too short for max.
	st := m.Stop(t0, 43*time.Second, t0.Add(23*time.Second))
	if st.Cores != 4 || st.Max == nil || st.Avg == nil {
		t.Fatalf("stats %+v", st)
	}
	if !approx(*st.Max, 75) || st.Samples != 2 {
		t.Errorf("max = %v (%d samples), want 75", *st.Max, st.Samples)
	}
	if w := st.Windows; len(w) != 2 || !approx(w[0].Pct, 25) || !approx(w[1].Pct, 75) || !w[1].Start.Equal(t0.Add(11*time.Second)) || !w[1].End.Equal(t0.Add(21*time.Second)) {
		t.Errorf("windows = %+v", w)
	}
	// avg = 43 s CPU / (23 s * 4 cores) from process start.
	if want := 43.0 / (23 * 4) * 100; !approx(*st.Avg, want) {
		t.Errorf("avg = %v, want %v", *st.Avg, want)
	}
}

func TestCPUMonitorNoFinal(t *testing.T) {
	m := &CPUMonitor{cores: 2, interval: time.Second}
	t0 := time.Unix(1000, 0)
	m.add(t0, 0)
	m.add(t0.Add(time.Second), time.Second)
	st := m.Stop(time.Time{}, 0, time.Time{})
	if st.Avg == nil || !approx(*st.Avg, 50) || st.Max == nil || !approx(*st.Max, 50) {
		t.Errorf("stats %+v", st)
	}
	empty := (&CPUMonitor{cores: 2, interval: time.Second}).Stop(time.Time{}, 0, time.Time{})
	if empty.Avg != nil || empty.Max != nil || empty.Cores != 2 {
		t.Errorf("empty stats %+v", empty)
	}
}

func TestCPUMonitorLiveSelf(t *testing.T) {
	m := NewCPUMonitor(os.Getpid(), 50*time.Millisecond)
	start := time.Now()
	m.Start()
	deadline := time.Now().Add(300 * time.Millisecond)
	x := 0.0
	for time.Now().Before(deadline) {
		x += math.Sqrt(x + 1) // burn CPU
	}
	_ = x
	st := m.Stop(start, 0, time.Time{})
	if st.Errors > 0 {
		t.Fatalf("ProcessCPUTime failed %d times", st.Errors)
	}
	if st.Avg == nil || st.Max == nil || *st.Max <= 0 || *st.Max > 100 {
		t.Fatalf("stats %+v", st)
	}
}

func TestProcessCPUTimeSelf(t *testing.T) {
	d, err := ProcessCPUTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if d < 0 {
		t.Errorf("cpu = %v", d)
	}
}

func TestParseProcStat(t *testing.T) {
	line := "4242 (k6 (x) y) S 1 4242 4242 0 -1 4194560 2000 0 0 0 1234 567 0 0 20 0 12 0 100 0 0"
	d, err := parseProcStat(line)
	if err != nil || d != 18010*time.Millisecond {
		t.Errorf("got %v %v, want 18.01s", d, err)
	}
	if _, err := parseProcStat("garbage"); err == nil {
		t.Error("want error")
	}
}

func TestParsePSTime(t *testing.T) {
	cases := map[string]time.Duration{
		"0:01.50\n":    1500 * time.Millisecond,
		"  12:03.25":   12*time.Minute + 3250*time.Millisecond,
		"01:02:03":     time.Hour + 2*time.Minute + 3*time.Second,
		"2-01:00:00":   49 * time.Hour,
		"00:00:00.00 ": 0,
	}
	for in, want := range cases {
		got, err := parsePSTime(in)
		if err != nil || got != want {
			t.Errorf("%q: got %v %v, want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "1:2:3:4", "x-1:00"} {
		if _, err := parsePSTime(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestPrometheusHeapFallback(t *testing.T) {
	for _, tc := range []struct {
		body string
		want float64
	}{
		{"process_resident_memory_bytes 1\nnodejs_heap_size_used_bytes 200\nnodejs_heap_used_bytes 100\n", 200},
		{"process_resident_memory_bytes 1\nnodejs_heap_used_bytes 100\n", 100},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(tc.body))
		}))
		p, err := NewPrometheus(srv.URL, DefaultPromNames()).Sample(context.Background())
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if p.HeapBytes == nil || *p.HeapBytes != tc.want {
			t.Errorf("heap = %v, want %v", p.HeapBytes, tc.want)
		}
	}
}
