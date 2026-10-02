//go:build windows

package k6run

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup starts k6 in a new console process group: a console Ctrl-C
// then reaches only mcpload, which forwards it as CTRL_BREAK (Windows cannot
// send CTRL_C to another process group; k6 handles CTRL_BREAK like Ctrl-C).
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

var procGenerateConsoleCtrlEvent = syscall.NewLazyDLL("kernel32.dll").NewProc("GenerateConsoleCtrlEvent")

// interruptProcess sends CTRL_BREAK to k6's process group (its pid). It fails
// when mcpload has no console; the caller then kills k6.
func interruptProcess(p *os.Process, _ os.Signal) error {
	if err := procGenerateConsoleCtrlEvent.Find(); err != nil {
		return err
	}
	r, _, err := procGenerateConsoleCtrlEvent.Call(syscall.CTRL_BREAK_EVENT, uintptr(p.Pid))
	if r == 0 {
		return err
	}
	return nil
}
