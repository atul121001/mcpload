//go:build linux

package client

import "syscall"

func init() {
	// The server dies with k6 even when k6 itself is SIGKILLed (Close never
	// runs then). Pdeathsig fires when the thread that started the process
	// exits; Go keeps its threads, so in practice that is when k6 exits.
	extraProcAttrs = func(a *syscall.SysProcAttr) { a.Pdeathsig = syscall.SIGKILL }
}
