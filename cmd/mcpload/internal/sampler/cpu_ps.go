//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package sampler

import (
	"os/exec"
	"strconv"
	"time"
)

// ProcessCPUTime returns the cumulative CPU time of pid from `ps -o time=`.
func ProcessCPUTime(pid int) (time.Duration, error) {
	out, err := exec.Command("ps", "-o", "time=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	return parsePSTime(string(out))
}
