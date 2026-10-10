//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package sampler

import (
	"context"
	"errors"
	"os/exec"
)

// readProcs runs `ps -o pid=,rss= -p <pids>` once for all processes. ps
// exits 1 when some (or all) of the pids are gone; it still lists the live
// ones. Open fds are not counted (nil).
func readProcs(ctx context.Context, pids []int) (map[int]procUsage, error) {
	out, err := exec.CommandContext(ctx, "ps", "-o", "pid=,rss=", "-p", joinPIDs(pids)).Output()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return nil, err
		}
	}
	return parsePSRSS(string(out))
}
