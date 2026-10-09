//go:build windows

package client

import (
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processTree is a job object holding the stdio server process. Children it
// starts (npx.cmd -> node, uvx -> python) join the job, and the job kills
// them all when it is terminated or when its last handle closes, so nothing
// outlives k6, even when k6 itself is killed.
type processTree struct {
	once sync.Once
	job  windows.Handle // 0 when the job could not be set up (kill falls back to the process)
	cmd  *exec.Cmd
}

func setProcAttrs(cmd *exec.Cmd) {
	// Own process group: a Ctrl+C in k6's console is not delivered to the
	// server; Close ends it instead.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

func attachProcessTree(cmd *exec.Cmd) *processTree {
	t := &processTree{cmd: cmd}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return t
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return t
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return t
	}
	err = windows.AssignProcessToJobObject(job, h)
	_ = windows.CloseHandle(h)
	if err != nil {
		_ = windows.CloseHandle(job)
		return t
	}
	t.job = job
	return t
}

func (t *processTree) kill() {
	if t.job != 0 {
		_ = windows.TerminateJobObject(t.job, 1)
		return
	}
	_ = t.cmd.Process.Kill()
}

// release closes the job handle once the process has been reaped (closing it
// kills whatever is left in the job).
func (t *processTree) release() {
	t.once.Do(func() {
		if t.job != 0 {
			_ = windows.CloseHandle(t.job)
		}
	})
}
