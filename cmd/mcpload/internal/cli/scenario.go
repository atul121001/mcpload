package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/engine"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/k6run"
)

// defaultScenario is the bundled scenario used when --scenario is omitted.
const defaultScenario = "agent-session"

// isFile reports whether p exists and is not a directory.
func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// isBareName reports whether s looks like a scenario name ("soak") rather
// than a path ("scenarios/soak.js", "./soak", "soak.js").
func isBareName(s string) bool {
	return s != "" && !strings.ContainsAny(s, `/\:`) && filepath.Ext(s) == ""
}

// resolveScenario turns the --scenario value into a script path.
//
//   - A value that names an existing file is used as is.
//   - An empty value means the bundled "agent-session" scenario.
//   - A bare name such as "soak" is looked up as scenarios/<name>.js in the
//     current folder (cwd), then next to the mcpload executable (exeDir),
//     which is how release archives ship it, then among the scenarios built
//     into release builds of mcpload.
//
// Any other value is a path that does not exist, which is an error as before.
func resolveScenario(arg, cwd, exeDir string) (string, error) {
	if arg != "" && isFile(arg) {
		return arg, nil
	}
	name := arg
	if name == "" {
		name = defaultScenario
	}
	if !isBareName(name) {
		if _, err := os.Stat(arg); err != nil {
			return "", fmt.Errorf("scenario: %w", err)
		}
		return "", fmt.Errorf("scenario: %s is a directory", arg)
	}
	rel := filepath.Join("scenarios", name+".js")
	var looked []string
	for _, dir := range []string{cwd, exeDir} {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, rel)
		if containsPath(looked, p) {
			continue
		}
		looked = append(looked, p)
		if isFile(p) {
			return p, nil
		}
	}
	if dir, err := builtinScenariosDir(); err == nil {
		p := filepath.Join(dir, rel)
		if isFile(p) {
			return p, nil
		}
		looked = append(looked, "the scenarios built into mcpload")
	}
	what := fmt.Sprintf("scenario %q", name)
	if arg == "" {
		what = fmt.Sprintf("no --scenario given and the default scenario %q", name)
	}
	return "", fmt.Errorf("%s was not found (looked for %s); pass --scenario <file.js> or run from a folder that contains scenarios/",
		what, strings.Join(looked, " and "))
}

func containsPath(list []string, p string) bool {
	for _, q := range list {
		if strings.EqualFold(filepath.Clean(q), filepath.Clean(p)) {
			return true
		}
	}
	return false
}

// builtinScenariosDir returns the folder holding the scenarios embedded in
// release builds (extracted on first use); a variable so tests can stub it.
var builtinScenariosDir = engine.ScenariosDir

// exeDir is the folder of the real (symlink-resolved) mcpload executable,
// or "" if unknown.
func exeDir() string { return k6run.ExeDir() }
