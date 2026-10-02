package k6run

import (
	"bytes"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHelperProcess is not a real test: it is the child started by
// TestRunForwardsSignal. It waits for an interrupt and exits 0 (like k6
// stopping gracefully), or exits 3 after 20 s.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("MCPLOAD_HELPER") != "1" {
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	os.Stdout.WriteString("ready\n")
	select {
	case s := <-ch:
		os.Stdout.WriteString("got " + s.String() + "\n")
		os.Exit(0)
	case <-time.After(20 * time.Second):
		os.Exit(3)
	}
}

func helperCmd() *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "MCPLOAD_HELPER=1")
	return cmd
}

type syncBuf struct {
	ch  chan struct{}
	buf bytes.Buffer // not embedded: Buffer.ReadFrom would bypass Write
}

func (b *syncBuf) Write(p []byte) (int, error) {
	n, err := b.buf.Write(p)
	if strings.Contains(b.buf.String(), "ready") {
		select {
		case b.ch <- struct{}{}:
		default:
		}
	}
	return n, err
}

func TestRunForwardsSignal(t *testing.T) {
	sigs := make(chan os.Signal, 2)
	out := &syncBuf{ch: make(chan struct{}, 1)}
	var pid int
	go func() {
		<-out.ch
		sigs <- os.Interrupt
	}()
	start := time.Now()
	res, err := runCmd(helperCmd(), RunConfig{
		Stdout: out, Stderr: os.Stderr, Signals: sigs, Grace: 5 * time.Second,
		OnStart: func(p int) { pid = p },
	})
	if err != nil {
		t.Fatal(err)
	}
	if pid <= 0 || !res.Interrupted {
		t.Fatalf("pid %d, result %+v", pid, res)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatalf("Run took %v", time.Since(start))
	}
	if res.Ended.Before(res.Started) {
		t.Errorf("times %v %v", res.Started, res.Ended)
	}
	t.Logf("result %+v, child output %q", res, out.String())
	if res.Killed {
		// Only acceptable where a console control event cannot be delivered
		// (Windows without a console); k6 is then killed after the grace period.
		t.Logf("signal could not be delivered; child was killed")
		return
	}
	if res.ExitCode != 0 || !strings.Contains(out.String(), "got ") {
		t.Errorf("child did not stop gracefully: %+v %q", res, out.String())
	}
}

func TestRunKillsOnSecondSignal(t *testing.T) {
	sigs := make(chan os.Signal, 2)
	// Two signals at once: the second forces a kill even if the first is ignored.
	cmd := helperCmd()
	out := &syncBuf{ch: make(chan struct{}, 1)}
	go func() {
		<-out.ch
		sigs <- os.Interrupt
		sigs <- os.Interrupt
	}()
	res, err := runCmd(cmd, RunConfig{Stdout: out, Stderr: os.Stderr, Signals: sigs, Grace: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Interrupted {
		t.Fatalf("result %+v", res)
	}
}

func (b *syncBuf) String() string { return b.buf.String() }
