package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain hides the built-in scenarios so lookups only see the test folders,
// also in -tags embedengine builds; tests that need them call stubBuiltin.
func TestMain(m *testing.M) {
	if os.Getenv(fakeK6Env) == "1" {
		// Started by a test as the engine (see stdio_run_test.go).
		os.Exit(fakeK6(os.Args[1:]))
	}
	builtinScenariosDir = func() (string, error) { return "", errors.New("no built-in scenarios in tests") }
	os.Exit(m.Run())
}

func writeScenario(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, "scenarios", name+".js")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("export default function () {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveScenarioDefaultPrefersCwd(t *testing.T) {
	cwd, exe := t.TempDir(), t.TempDir()
	want := writeScenario(t, cwd, "agent-session")
	writeScenario(t, exe, "agent-session")
	got, err := resolveScenario("", cwd, exe)
	if err != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
}

func TestResolveScenarioDefaultNextToExe(t *testing.T) {
	cwd, exe := t.TempDir(), t.TempDir()
	want := writeScenario(t, exe, "agent-session")
	got, err := resolveScenario("", cwd, exe)
	if err != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
}

func TestResolveScenarioBareName(t *testing.T) {
	cwd, exe := t.TempDir(), t.TempDir()
	writeScenario(t, cwd, "agent-session")
	soak := writeScenario(t, exe, "soak")
	lb := writeScenario(t, cwd, "lb-check")
	for arg, want := range map[string]string{"soak": soak, "lb-check": lb} {
		got, err := resolveScenario(arg, cwd, exe)
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", arg, got, err, want)
		}
		if base := strings.TrimSuffix(filepath.Base(got), filepath.Ext(got)); base != arg {
			t.Errorf("%s: scenario name from path = %q", arg, base)
		}
	}
}

func TestResolveScenarioExplicitPath(t *testing.T) {
	cwd, exe := t.TempDir(), t.TempDir()
	p := writeScenario(t, t.TempDir(), "custom")
	got, err := resolveScenario(p, cwd, exe)
	if err != nil || got != p {
		t.Fatalf("got %q, %v; want %q", got, err, p)
	}
	missing := filepath.Join(cwd, "nope.js")
	if _, err := resolveScenario(missing, cwd, exe); err == nil || !strings.Contains(err.Error(), "scenario:") {
		t.Fatalf("missing explicit path: %v", err)
	}
}

func TestResolveScenarioNotFound(t *testing.T) {
	cwd, exe := t.TempDir(), t.TempDir()
	_, err := resolveScenario("", cwd, exe)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, s := range []string{
		filepath.Join(cwd, "scenarios", "agent-session.js"),
		filepath.Join(exe, "scenarios", "agent-session.js"),
		"--scenario",
	} {
		if !strings.Contains(msg, s) {
			t.Errorf("error %q does not mention %q", msg, s)
		}
	}
	if _, err := resolveScenario("soak", cwd, exe); err == nil || !strings.Contains(err.Error(), `"soak"`) {
		t.Errorf("bare name not found: %v", err)
	}
}

func TestResolveScenarioSameDirLookedOnce(t *testing.T) {
	dir := t.TempDir()
	_, err := resolveScenario("", dir, dir)
	if err == nil || strings.Count(err.Error(), "agent-session.js") != 1 {
		t.Fatalf("got %v", err)
	}
}

func stubBuiltin(t *testing.T, dir string, err error) {
	t.Helper()
	old := builtinScenariosDir
	builtinScenariosDir = func() (string, error) { return dir, err }
	t.Cleanup(func() { builtinScenariosDir = old })
}

func TestResolveScenarioBuiltinFallback(t *testing.T) {
	cwd, exe, builtin := t.TempDir(), t.TempDir(), t.TempDir()
	want := writeScenario(t, builtin, "soak")
	stubBuiltin(t, builtin, nil)
	got, err := resolveScenario("soak", cwd, exe)
	if err != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
	// An on-disk scenarios/ folder still wins over the built-in copy.
	disk := writeScenario(t, cwd, "soak")
	if got, err := resolveScenario("soak", cwd, exe); err != nil || got != disk {
		t.Fatalf("got %q, %v; want %q", got, err, disk)
	}
	// A name that is not built in either reports the built-in lookup too.
	if _, err := resolveScenario("nope", cwd, exe); err == nil || !strings.Contains(err.Error(), "built into mcpload") {
		t.Fatalf("got %v", err)
	}
}
