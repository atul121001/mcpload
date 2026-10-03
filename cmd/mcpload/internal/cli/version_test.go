package cli

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/engine"
)

func TestFindScenariosDir(t *testing.T) {
	stubBuiltin(t, "", errors.New("none"))
	cwd, exe := t.TempDir(), t.TempDir()
	if got, _ := findScenariosDir(cwd, exe); got != "" {
		t.Fatalf("empty: got %q", got)
	}
	writeScenario(t, exe, "agent-session")
	if got, want := pick(findScenariosDir(cwd, exe)), filepath.Join(exe, "scenarios"); got != want {
		t.Fatalf("exe: got %q, want %q", got, want)
	}
	writeScenario(t, cwd, "agent-session")
	if got, want := pick(findScenariosDir(cwd, exe)), filepath.Join(cwd, "scenarios"); got != want {
		t.Fatalf("cwd first: got %q, want %q", got, want)
	}
	if got, _ := findScenariosDir("", ""); got != "" {
		t.Fatalf("no dirs: got %q", got)
	}
	builtin := t.TempDir()
	stubBuiltin(t, builtin, nil)
	if got, isBuiltin := findScenariosDir("", ""); got != filepath.Join(builtin, "scenarios") || !isBuiltin {
		t.Fatalf("builtin: got %q, %v", got, isBuiltin)
	}
}

func pick(dir string, _ bool) string { return dir }

func TestVersionReportsMissingInstall(t *testing.T) {
	// From an empty folder with no engine on PATH, version still exits 0 and
	// says what is missing (the test binary's folder has neither k6 nor
	// scenarios/, and test builds embed nothing).
	if engine.Embedded() {
		t.Skip("built with -tags embedengine")
	}
	stubBuiltin(t, "", errors.New("none"))
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("PATH", dir)
	t.Setenv("MCPLOAD_ENGINE", "")
	var out, errb bytes.Buffer
	if c := Main([]string{"version"}, &out, &errb); c != ExitPass {
		t.Fatalf("exit %d", c)
	}
	s := out.String()
	for _, want := range []string{"mcpload " + Version + "\n", "  engine:    not found", "  scenarios: not found"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func TestVersionReportsScenariosInCwd(t *testing.T) {
	dir := t.TempDir()
	writeScenario(t, dir, "agent-session")
	t.Chdir(dir)
	var out, errb bytes.Buffer
	if c := Main([]string{"version"}, &out, &errb); c != ExitPass {
		t.Fatalf("exit %d", c)
	}
	if !strings.Contains(out.String(), "  scenarios: "+filepath.Join(dir, "scenarios")+"\n") {
		t.Errorf("scenarios line missing:\n%s", out.String())
	}
}
