package k6run

import (
	"os"
	"path/filepath"
	"testing"
)

// install.sh and Homebrew put a symlink to mcpload on PATH; "next to mcpload"
// must mean next to the real binary, where k6 and scenarios/ live.
func TestRealDirFollowsSymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "mcpload_0.3.0", "mcpload")
	bin := filepath.Join(root, "bin")
	for _, d := range []string{filepath.Dir(real), bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(bin, "mcpload")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks not available here: %v", err)
	}
	want, err := filepath.EvalSymlinks(filepath.Dir(real))
	if err != nil {
		t.Fatal(err)
	}
	if got := RealDir(link); got != want {
		t.Fatalf("RealDir(link) = %q, want %q", got, want)
	}
}

func TestRealDirPlainFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mcpload")
	if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := RealDir(p); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// A path that cannot be resolved falls back to its own folder.
	missing := filepath.Join(dir, "nope", "mcpload")
	if got := RealDir(missing); got != filepath.Dir(missing) {
		t.Fatalf("missing: got %q", got)
	}
}
