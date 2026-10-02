//go:build unix

package k6run

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup puts k6 in its own process group, so a terminal Ctrl-C
// (sent to the foreground group) reaches only mcpload, which forwards it
// once; k6 treats a second SIGINT as "abort now".
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// interruptProcess forwards sig (SIGINT or SIGTERM) to k6.
func interruptProcess(p *os.Process, sig os.Signal) error {
	if sig == nil {
		sig = os.Interrupt
	}
	return p.Signal(sig)
}
