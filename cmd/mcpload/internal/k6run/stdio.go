package k6run

import (
	"sort"
	"strconv"
)

// Metrics xk6-mcpload emits for the stdio transport (one server process per
// session). Every sample of the extension also carries a transport tag
// (stdio or http).
const (
	MetricProcessSpawnDuration = "mcp_process_spawn_duration" // Trend, ms
	MetricProcessesOpen        = "mcp_processes_open"         // Gauge
	MetricStdoutInvalidLines   = "mcp_stdout_invalid_lines"   // Counter
	MetricProcessExits         = "mcp_process_exits"          // Counter, tags expected=true|false, exit_code

	TransportTag = "transport"
)

// procAgg collects the stdio process metrics.
type procAgg struct {
	spawns       []float64
	maxOpen      float64
	haveOpen     bool
	invalidLines float64
	exits        float64
	unexpected   float64
	exitCodes    map[string]float64 // unexpected exits by exit_code
	samples      int
}

func (p *procAgg) add(metric string, v float64, tags map[string]string) {
	p.samples++
	switch metric {
	case MetricProcessSpawnDuration:
		p.spawns = append(p.spawns, v)
	case MetricProcessesOpen:
		if !p.haveOpen || v > p.maxOpen {
			p.maxOpen, p.haveOpen = v, true
		}
	case MetricStdoutInvalidLines:
		p.invalidLines += v
	case MetricProcessExits:
		p.exits += v
		if tags["expected"] == "false" {
			p.unexpected += v
			if p.exitCodes == nil {
				p.exitCodes = map[string]float64{}
			}
			code := tags["exit_code"]
			if code == "" {
				code = "unknown"
			}
			p.exitCodes[code] += v
		}
	}
}

// ProcessStats summarises the server processes of a stdio run.
type ProcessStats struct {
	// Spawns is mcp_process_spawn_duration (ms): one sample per started process.
	Spawns Latency
	// MaxOpen is the highest mcp_processes_open value seen.
	MaxOpen int64
	// InvalidLines counts stdout lines that were not JSON-RPC messages.
	InvalidLines int64
	// Exits counts every process exit; Unexpected those tagged expected=false,
	// split by exit code in ExitCodes (sorted by count, then code).
	Exits, Unexpected int64
	ExitCodes         []ExitCodeCount
}

// ExitCodeCount is one exit code of unexpected process exits.
type ExitCodeCount struct {
	Code  string
	Count int64
}

// Processes returns the stdio process stats, or nil when the run emitted
// none of the process metrics.
func (a *Aggregator) Processes() *ProcessStats {
	p := &a.proc
	if p.samples == 0 {
		return nil
	}
	st := &ProcessStats{Spawns: latencyOf(p.spawns), MaxOpen: round(p.maxOpen), InvalidLines: round(p.invalidLines),
		Exits: round(p.exits), Unexpected: round(p.unexpected)}
	for c, n := range p.exitCodes {
		st.ExitCodes = append(st.ExitCodes, ExitCodeCount{Code: c, Count: round(n)})
	}
	sort.Slice(st.ExitCodes, func(i, j int) bool {
		x, y := st.ExitCodes[i], st.ExitCodes[j]
		if x.Count != y.Count {
			return x.Count > y.Count
		}
		xi, xerr := strconv.Atoi(x.Code)
		yi, yerr := strconv.Atoi(y.Code)
		if xerr == nil && yerr == nil {
			return xi < yi
		}
		return x.Code < y.Code
	})
	return st
}
