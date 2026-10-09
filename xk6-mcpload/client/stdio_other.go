//go:build !unix && !windows

package client

import "os/exec"

// processTree is just the server process on platforms without process groups
// or job objects: its children are not tracked.
type processTree struct{ cmd *exec.Cmd }

func setProcAttrs(*exec.Cmd) {}

func attachProcessTree(cmd *exec.Cmd) *processTree { return &processTree{cmd: cmd} }

func (t *processTree) kill() { _ = t.cmd.Process.Kill() }

func (t *processTree) release() {}
