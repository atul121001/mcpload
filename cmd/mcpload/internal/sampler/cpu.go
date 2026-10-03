package sampler

import (
	"context"
	"math"
	"runtime"
	"sync"
	"time"
)

// CPUPercent converts cpu time used over a wall-clock window into a share of
// the machine's total CPU capacity (0-100): cpu / (wall * cores) * 100.
// It returns NaN for an empty window.
func CPUPercent(cpu, wall time.Duration, cores int) float64 {
	if wall <= 0 || cores <= 0 {
		return math.NaN()
	}
	p := float64(cpu) / (float64(wall) * float64(cores)) * 100
	return math.Max(0, math.Min(100, p))
}

// CPUStats summarise a process's CPU use as % of total machine capacity.
// Avg and Max are nil when unknown.
type CPUStats struct {
	Cores    int
	Avg, Max *float64
	Samples  int // interval readings that contributed to Max
	Errors   int // failed readings
	// Windows are the readings that contributed to Max, in time order, so a
	// caller can find the peak within part of the run (e.g. one load step).
	Windows []CPUWindow
}

// CPUWindow is the CPU share (0-100) of one sampling window [Start, End).
type CPUWindow struct {
	Start, End time.Time
	Pct        float64
}

// CPUMonitor samples one process's cumulative CPU time every interval.
type CPUMonitor struct {
	pid      int
	cores    int
	interval time.Duration
	read     func(pid int) (time.Duration, error)

	mu        sync.Mutex
	startWall time.Time
	startCPU  time.Duration
	haveStart bool
	lastWall  time.Time
	lastCPU   time.Duration
	max       float64
	samples   int
	windows   []CPUWindow
	errs      int

	cancel context.CancelFunc
	done   chan struct{}
}

// NewCPUMonitor returns a monitor for pid, normalised to runtime.NumCPU() cores.
func NewCPUMonitor(pid int, interval time.Duration) *CPUMonitor {
	if interval <= 0 {
		interval = time.Second
	}
	return &CPUMonitor{pid: pid, cores: runtime.NumCPU(), interval: interval, read: ProcessCPUTime}
}

// Start takes a baseline reading and then samples every interval until Stop.
func (m *CPUMonitor) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.done = make(chan struct{})
	m.observe(time.Now())
	go func() {
		defer close(m.done)
		tk := time.NewTicker(m.interval)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tk.C:
				m.observe(now)
			}
		}
	}()
}

func (m *CPUMonitor) observe(now time.Time) {
	cpu, err := m.read(m.pid)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.errs++
		return
	}
	m.add(now, cpu)
}

// add records one cumulative reading; m.mu must be held. Windows shorter
// than half an interval do not count towards Max (too noisy).
func (m *CPUMonitor) add(now time.Time, cpu time.Duration) {
	if !m.haveStart {
		m.startWall, m.startCPU, m.haveStart = now, cpu, true
		m.lastWall, m.lastCPU = now, cpu
		return
	}
	dw := now.Sub(m.lastWall)
	if dw >= m.interval/2 {
		if p := CPUPercent(cpu-m.lastCPU, dw, m.cores); !math.IsNaN(p) {
			if m.samples == 0 || p > m.max {
				m.max = p
			}
			m.samples++
			m.windows = append(m.windows, CPUWindow{Start: m.lastWall, End: now, Pct: p})
		}
	}
	m.lastWall, m.lastCPU = now, cpu
}

// Stop ends sampling and returns the stats. start is the process start time
// (CPU time 0 then); when zero, the first reading is the baseline. When the
// process has exited, pass its total CPU time (os.ProcessState user+system)
// and exit time as finalCPU/finalAt for an exact average and a last window;
// a zero finalAt uses the last periodic reading only.
func (m *CPUMonitor) Stop(start time.Time, finalCPU time.Duration, finalAt time.Time) CPUStats {
	if m.cancel != nil {
		m.cancel()
		<-m.done
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := CPUStats{Cores: m.cores, Errors: m.errs}
	startWall, startCPU, haveStart := m.startWall, m.startCPU, m.haveStart
	if !start.IsZero() {
		startWall, startCPU, haveStart = start, 0, true
	}
	endWall, endCPU := m.lastWall, m.lastCPU
	if !finalAt.IsZero() && finalCPU > 0 {
		if m.haveStart && finalCPU >= m.lastCPU {
			m.add(finalAt, finalCPU)
		}
		endWall, endCPU = finalAt, finalCPU
	}
	if haveStart && endWall.After(startWall) {
		if p := CPUPercent(endCPU-startCPU, endWall.Sub(startWall), m.cores); !math.IsNaN(p) {
			st.Avg = fp(p)
		}
	}
	if m.samples > 0 {
		st.Max = fp(m.max)
		st.Samples = m.samples
		st.Windows = append([]CPUWindow(nil), m.windows...)
	}
	if st.Avg != nil && (st.Max == nil || *st.Max < *st.Avg) {
		// Only possible through short windows; the peak is at least the mean.
		st.Max = fp(*st.Avg)
	}
	return st
}
