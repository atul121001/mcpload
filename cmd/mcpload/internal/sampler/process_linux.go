//go:build linux

package sampler

import (
	"context"
	"fmt"
	"os"
)

// readProcs reads VmRSS from /proc/<pid>/status and counts /proc/<pid>/fd.
// Processes that are gone are skipped.
func readProcs(_ context.Context, pids []int) (map[int]procUsage, error) {
	m := map[int]procUsage{}
	for _, pid := range pids {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			continue // exited
		}
		rss, ok, err := parseVmRSS(string(b))
		if err != nil {
			return nil, fmt.Errorf("process sampler: pid %d: %w", pid, err)
		}
		if !ok {
			continue // zombie
		}
		u := procUsage{RSS: rss}
		if ents, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid)); err == nil {
			u.FDs = fp(float64(len(ents)))
		}
		m[pid] = u
	}
	return m, nil
}
