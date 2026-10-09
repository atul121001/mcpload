package sampler

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func TestParseVmRSS(t *testing.T) {
	status := "Name:\tnode\nVmPeak:\t  999 kB\nVmRSS:\t   51200 kB\nThreads:\t7\n"
	v, ok, err := parseVmRSS(status)
	if err != nil || !ok || v != 51200*1024 {
		t.Fatalf("got %v %v %v", v, ok, err)
	}
	if _, ok, err := parseVmRSS("Name:\tkthreadd\nState:\tS\n"); ok || err != nil {
		t.Fatalf("no VmRSS line: ok=%v err=%v", ok, err)
	}
	if _, _, err := parseVmRSS("VmRSS:\tlots kB\n"); err == nil {
		t.Fatal("bad number accepted")
	}
	if _, _, err := parseVmRSS("VmRSS:\t12 MB\n"); err == nil {
		t.Fatal("unexpected unit accepted")
	}
}

func TestParsePSRSS(t *testing.T) {
	m, err := parsePSRSS("  123  2048\n 4567 100\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || m[123].RSS != 2048*1024 || m[4567].RSS != 100*1024 || m[123].FDs != nil {
		t.Fatalf("got %+v", m)
	}
	if _, err := parsePSRSS("123\n"); err == nil {
		t.Fatal("short line accepted")
	}
	if m, err := parsePSRSS(""); err != nil || len(m) != 0 {
		t.Fatalf("empty: %v %v", m, err)
	}
}

func TestListPIDs(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"42", "7", "notapid", "-3", "0"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "99"), 0o755); err != nil {
		t.Fatal(err)
	}
	pids, err := ListPIDs(dir)
	if err != nil || len(pids) != 2 || pids[0] != 7 || pids[1] != 42 {
		t.Fatalf("got %v %v", pids, err)
	}
	if pids, err := ListPIDs(filepath.Join(dir, "missing")); err != nil || pids != nil {
		t.Fatalf("missing dir: %v %v", pids, err)
	}
}

func TestSumUsage(t *testing.T) {
	now := time.Now()
	p := sumUsage(now, map[int]procUsage{1: {RSS: 10, FDs: fp(3)}, 2: {RSS: 5}})
	if *p.RSSBytes != 15 || *p.OpenFDs != 3 {
		t.Fatalf("got rss %v fds %v", *p.RSSBytes, *p.OpenFDs)
	}
	p = sumUsage(now, map[int]procUsage{1: {RSS: 10}})
	if p.OpenFDs != nil {
		t.Fatal("fds without any count")
	}
	if p = sumUsage(now, nil); p.RSSBytes != nil {
		t.Fatal("rss without processes")
	}
}

func TestProcessSamplerNoProcesses(t *testing.T) {
	s := NewProcess(t.TempDir())
	if s.Kind() != "process" {
		t.Fatal(s.Kind())
	}
	p, err := s.Sample(context.Background())
	if err != nil || p.RSSBytes != nil || p.OpenFDs != nil || p.T.IsZero() {
		t.Fatalf("got %+v %v", p, err)
	}
}

// TestProcessSamplerOwnPID samples the test process itself, plus a pid that
// does not exist (skipped).
func TestProcessSamplerOwnPID(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "windows", "darwin", "freebsd", "netbsd", "openbsd", "dragonfly":
	default:
		t.Skip("process sampler not supported on " + runtime.GOOS)
	}
	dir := t.TempDir()
	for _, pid := range []int{os.Getpid(), 0x7ffffff0} {
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := NewProcess(dir).Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.RSSBytes == nil || *p.RSSBytes < 1<<20 {
		t.Fatalf("own RSS: %v", p.RSSBytes)
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "windows" {
		if p.OpenFDs == nil || *p.OpenFDs < 3 {
			t.Fatalf("own fds/handles: %v", p.OpenFDs)
		}
	}
	t.Logf("rss %.1f MiB, fds %v", *p.RSSBytes/(1<<20), deref(p.OpenFDs))
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
