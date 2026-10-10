//go:build windows

package sampler

import (
	"context"
	"syscall"
	"unsafe"
)

var (
	modKernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procK32GetProcessMemoryInfo = modKernel32.NewProc("K32GetProcessMemoryInfo")
	procGetProcessHandleCount   = modKernel32.NewProc("GetProcessHandleCount")
)

// processMemoryCounters is PROCESS_MEMORY_COUNTERS.
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// stillActive is STILL_ACTIVE, the exit code of a running process.
const stillActive = 259

// readProcs reads each process's working set (GetProcessMemoryInfo, the
// Windows counterpart of RSS) and handle count (GetProcessHandleCount, the
// counterpart of open fds). Processes that are gone are skipped.
func readProcs(_ context.Context, pids []int) (map[int]procUsage, error) {
	m := map[int]procUsage{}
	for _, pid := range pids {
		if u, ok := readProc(pid); ok {
			m[pid] = u
		}
	}
	return m, nil
}

func readProc(pid int) (procUsage, bool) {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return procUsage{}, false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil || code != stillActive {
		return procUsage{}, false
	}
	var pmc processMemoryCounters
	pmc.CB = uint32(unsafe.Sizeof(pmc))
	if r, _, _ := procK32GetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.CB)); r == 0 {
		return procUsage{}, false
	}
	u := procUsage{RSS: float64(pmc.WorkingSetSize)}
	var handles uint32
	if r, _, _ := procGetProcessHandleCount.Call(uintptr(h), uintptr(unsafe.Pointer(&handles))); r != 0 {
		u.FDs = fp(float64(handles))
	}
	return u, true
}
