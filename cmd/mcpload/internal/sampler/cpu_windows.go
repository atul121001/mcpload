//go:build windows

package sampler

import (
	"syscall"
	"time"
)

// processQueryLimitedInformation is PROCESS_QUERY_LIMITED_INFORMATION (not in package syscall).
const processQueryLimitedInformation = 0x1000

// ProcessCPUTime returns the cumulative user+kernel CPU time of pid.
func ProcessCPUTime(pid int) (time.Duration, error) {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	defer syscall.CloseHandle(h)
	var created, exited, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0, err
	}
	return filetimeDuration(kernel) + filetimeDuration(user), nil
}

// filetimeDuration converts a FILETIME used as a duration (100 ns units).
func filetimeDuration(f syscall.Filetime) time.Duration {
	return time.Duration(int64(f.HighDateTime)<<32|int64(f.LowDateTime)) * 100
}
