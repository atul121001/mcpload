package sampler

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PIDDirEnv names the directory where xk6-mcpload records the live server
// processes of a stdio run: one file per process, named by its pid, removed
// when the process exits.
const PIDDirEnv = "MCPLOAD_PID_DIR"

// procUsage is one process's resource use. FDs is nil when the platform (or
// a permission problem) does not let mcpload count them.
type procUsage struct {
	RSS float64
	FDs *float64
}

type processSampler struct {
	dir  string
	read func(ctx context.Context, pids []int) (map[int]procUsage, error)
}

// NewProcess returns a sampler that sums resident memory (RSSBytes) and open
// file descriptors / handles (OpenFDs) over the live processes listed in dir
// (see PIDDirEnv). Processes that exited between listing and reading are
// skipped. With no live process the point has no values (nil, not 0), so
// the time before the first spawn does not read as a memory ramp.
//
// Per OS: Linux reads /proc/<pid>/status (VmRSS) and counts /proc/<pid>/fd;
// Windows uses the working set (GetProcessMemoryInfo) and the handle count
// (GetProcessHandleCount); macOS and the BSDs run `ps -o pid=,rss= -p ...`
// once per sample and report no fd count (lsof per process per sample is
// too slow to be worth it).
func NewProcess(dir string) Sampler { return &processSampler{dir: dir, read: readProcs} }

func (p *processSampler) Kind() string { return "process" }

func (p *processSampler) Sample(ctx context.Context) (Point, error) {
	t := time.Now()
	pids, err := ListPIDs(p.dir)
	if err != nil {
		return Point{}, err
	}
	if len(pids) == 0 {
		return Point{T: t}, nil
	}
	use, err := p.read(ctx, pids)
	if err != nil {
		return Point{}, err
	}
	return sumUsage(t, use), nil
}

// sumUsage adds up the processes' usage. OpenFDs is set when at least one
// process reported a count.
func sumUsage(t time.Time, use map[int]procUsage) Point {
	pt := Point{T: t}
	if len(use) == 0 {
		return pt
	}
	var rss, fds float64
	haveFDs := false
	for _, u := range use {
		rss += u.RSS
		if u.FDs != nil {
			fds += *u.FDs
			haveFDs = true
		}
	}
	pt.RSSBytes = fp(rss)
	if haveFDs {
		pt.OpenFDs = fp(fds)
	}
	return pt
}

// ListPIDs returns the pids recorded in dir (file names that are positive
// integers), sorted. A missing dir means no processes.
func ListPIDs(dir string) ([]int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("process sampler: %w", err)
	}
	var pids []int
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if n, err := strconv.Atoi(e.Name()); err == nil && n > 0 {
			pids = append(pids, n)
		}
	}
	sort.Ints(pids)
	return pids, nil
}

// parseVmRSS returns VmRSS from /proc/<pid>/status in bytes. ok is false when
// the line is missing (a zombie or kernel thread has none).
func parseVmRSS(status string) (bytes float64, ok bool, err error) {
	sc := bufio.NewScanner(strings.NewReader(status))
	for sc.Scan() {
		line := sc.Text()
		rest, found := strings.CutPrefix(line, "VmRSS:")
		if !found {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			return 0, false, fmt.Errorf("bad VmRSS line %q", line)
		}
		v, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			return 0, false, fmt.Errorf("bad VmRSS line %q", line)
		}
		mult := 1024.0 // the kernel always writes kB
		if len(f) > 1 && !strings.EqualFold(f[1], "kB") {
			return 0, false, fmt.Errorf("unexpected VmRSS unit in %q", line)
		}
		return v * mult, true, nil
	}
	return 0, false, nil
}

// parsePSRSS parses `ps -o pid=,rss=` output (rss in KiB) into usage per pid.
func parsePSRSS(out string) (map[int]procUsage, error) {
	m := map[int]procUsage{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 2 {
			return nil, fmt.Errorf("unexpected ps line %q", line)
		}
		pid, err1 := strconv.Atoi(f[0])
		kb, err2 := strconv.ParseFloat(f[1], 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("unexpected ps line %q", line)
		}
		m[pid] = procUsage{RSS: kb * 1024}
	}
	return m, nil
}

func joinPIDs(pids []int) string {
	s := make([]string, len(pids))
	for i, p := range pids {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ",")
}
