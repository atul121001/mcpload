//go:build unix

package client

import (
	"os/exec"
	"syscall"
)

// processTree is the stdio server's process group: the server is started as
// a group leader, so killing the group also kills the children of launchers
// such as npx, uvx or a shell script.
type processTree struct{ pgid int }

// extraProcAttrs adds OS-specific attributes (Linux: Pdeathsig).
var extraProcAttrs = func(*syscall.SysProcAttr) {}

func setProcAttrs(cmd *exec.Cmd) {
	attr := &syscall.SysProcAttr{Setpgid: true}
	extraProcAttrs(attr)
	cmd.SysProcAttr = attr
}

func attachProcessTree(cmd *exec.Cmd) *processTree {
	return &processTree{pgid: cmd.Process.Pid}
}

// kill sends SIGKILL to the whole group (ESRCH once it is empty is ignored).
func (t *processTree) kill() {
	if t.pgid > 0 {
		_ = syscall.Kill(-t.pgid, syscall.SIGKILL)
	}
}

func (t *processTree) release() {}
