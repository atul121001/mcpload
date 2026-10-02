//go:build !linux && !windows && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package sampler

import (
	"errors"
	"time"
)

// ProcessCPUTime is not supported on this platform.
func ProcessCPUTime(int) (time.Duration, error) {
	return 0, errors.New("process CPU sampling is not supported on this platform")
}
