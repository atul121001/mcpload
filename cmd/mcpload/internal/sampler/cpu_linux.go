//go:build linux

package sampler

import (
	"fmt"
	"os"
	"time"
)

// ProcessCPUTime returns the cumulative user+system CPU time of pid.
func ProcessCPUTime(pid int) (time.Duration, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	return parseProcStat(string(b))
}
