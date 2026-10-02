package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
