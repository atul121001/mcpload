//go:build !unix && !windows

package k6run

import (
	"os"
	"os/exec"
)

func setProcessGroup(*exec.Cmd) {}

func interruptProcess(p *os.Process, _ os.Signal) error { return p.Signal(os.Interrupt) }
