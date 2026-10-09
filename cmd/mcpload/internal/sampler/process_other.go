//go:build !linux && !windows && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package sampler

import (
	"context"
	"errors"
)

// readProcs is not supported on this platform.
func readProcs(context.Context, []int) (map[int]procUsage, error) {
	return nil, errors.New("the process sampler is not supported on this platform")
}
