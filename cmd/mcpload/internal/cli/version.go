package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/k6run"
)

// versionCmd prints the mcpload version plus the engine and scenarios a run
// would use, so users can check an install from any folder. It always exits
// 0: a missing engine is reported, not treated as an error.
func versionCmd(stdout io.Writer) int {
	fmt.Fprintf(stdout, "mcpload %s\n", Version)
	cwd, _ := os.Getwd()

	if bin, source, err := k6run.Locate(""); err != nil {
		fmt.Fprintf(stdout, "  engine:    not found; install a release build of mcpload (see the README)\n")
	} else if source == k6run.SourceEmbedded {
		fmt.Fprintf(stdout, "  engine:    %s (embedded)\n", engineDetail(bin))
	} else {
		fmt.Fprintf(stdout, "  engine:    %s (%s, from %s)\n", engineDetail(bin), bin, source)
	}

	if dir, builtin := findScenariosDir(cwd, exeDir()); dir == "" {
		fmt.Fprintf(stdout, "  scenarios: not found (looked for scenarios/ in the current folder and next to mcpload); pass --scenario <file.js> to run\n")
	} else if builtin {
		fmt.Fprintf(stdout, "  scenarios: built in (%s)\n", dir)
	} else {
		fmt.Fprintf(stdout, "  scenarios: %s\n", dir)
	}
	return ExitPass
}

// engineDetail runs `<engine> version` and describes it, e.g.
// "k6 v2.3.0 with xk6-mcpload", with a warning when the binary was not built
// with the extension.
func engineDetail(bin string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version").Output()
	if err != nil {
		return "could not run it: " + err.Error()
	}
	v := "k6 " + k6run.ParseVersion(string(out))
	if !strings.Contains(string(out), "k6/x/mcpload") {
		return v + " WITHOUT xk6-mcpload (MCP scenarios will not run)"
	}
	return v + " with xk6-mcpload"
}

// findScenariosDir returns the scenarios/ folder a bare --scenario name would
// be looked up in: cwd first, then next to mcpload, then the scenarios built
// into release builds (builtin = true); "" if there is none.
func findScenariosDir(cwd, exeDir string) (dir string, builtin bool) {
	for _, d := range []string{cwd, exeDir} {
		if d == "" {
			continue
		}
		p := filepath.Join(d, "scenarios")
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p, false
		}
	}
	if d, err := builtinScenariosDir(); err == nil {
		return filepath.Join(d, "scenarios"), true
	}
	return "", false
}
