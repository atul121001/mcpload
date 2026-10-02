// Package k6run starts the k6 binary (built with xk6-mcpload) and turns its
// outputs (NDJSON metric stream, --summary-export JSON, `k6 inspect` options)
// into the aggregates the mcpload report needs.
package k6run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// FindBinary resolves the k6 binary: an explicit path wins; otherwise ./k6.exe
// (Windows) or ./k6 in the working directory, then k6 on PATH.
func FindBinary(explicit string) (string, error) {
	if explicit != "" {
		if p, err := exec.LookPath(explicit); err == nil {
			return p, nil
		}
		if st, err := os.Stat(explicit); err == nil && !st.IsDir() {
			return filepath.Abs(explicit)
		}
		return "", fmt.Errorf("k6 binary %q not found", explicit)
	}
	local := []string{"k6"}
	if runtime.GOOS == "windows" {
		local = []string{"k6.exe", "k6"}
	}
	for _, name := range local {
		if st, err := os.Stat(name); err == nil && !st.IsDir() {
			return filepath.Abs(name)
		}
	}
	p, err := exec.LookPath("k6")
	if err != nil {
		return "", errors.New("k6 binary not found (looked for ./k6.exe, ./k6 and k6 on PATH); build it with xk6 or pass --k6")
	}
	return p, nil
}

var versionRe = regexp.MustCompile(`v?\d+\.\d+\.\d+[0-9A-Za-z.+-]*`)

// Version runs `k6 version` and returns the version string (e.g. "v2.3.0").
func Version(ctx context.Context, bin string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, "version").Output()
	if err != nil {
		return "", fmt.Errorf("k6 version: %w", err)
	}
	return parseVersion(string(out)), nil
}

func parseVersion(out string) string {
	line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	if m := versionRe.FindString(line); m != "" {
		return m
	}
	if line == "" {
		return "unknown"
	}
	return line
}

// EnvArgs turns KEY=VALUE pairs into k6 `-e` flags.
func EnvArgs(env []string) []string {
	args := make([]string, 0, 2*len(env))
	for _, kv := range env {
		args = append(args, "-e", kv)
	}
	return args
}

// Inspect runs `k6 inspect` on the script with the given env and parses the options.
func Inspect(ctx context.Context, bin, script string, env []string) (*Options, error) {
	args := append([]string{"inspect"}, EnvArgs(env)...)
	args = append(args, script)
	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("k6 inspect: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return ParseOptions(out)
}

// RunConfig configures one `k6 run`.
type RunConfig struct {
	Bin         string
	Script      string
	Env         []string // KEY=VALUE, passed as -e
	NDJSONPath  string   // --out json=<path>
	SummaryPath string   // --summary-export=<path>
	ExtraArgs   []string // extra k6 run flags, before the script
	Stdout      io.Writer
	Stderr      io.Writer
	// OnStart is called right after the process started.
	OnStart func()
}

// SummaryTrendStats is passed to k6 so the summary export carries the common
// percentiles; observed values are computed from the NDJSON stream anyway.
const SummaryTrendStats = "avg,min,med,max,p(90),p(95),p(99),count"

// Args builds the k6 command line (without the binary).
func (c RunConfig) Args() []string {
	args := []string{"run",
		"--out", "json=" + c.NDJSONPath,
		"--summary-export", c.SummaryPath,
		"--summary-trend-stats", SummaryTrendStats,
	}
	args = append(args, EnvArgs(c.Env)...)
	args = append(args, c.ExtraArgs...)
	return append(args, c.Script)
}

// Exit codes k6 uses that still mean "the test ran to completion".
const (
	ExitOK               = 0
	ExitThresholdsFailed = 99
)

// Run executes k6 and waits. It returns k6's exit code; err is set only when
// the process could not be started or waited for.
func Run(c RunConfig) (int, error) {
	cmd := exec.Command(c.Bin, c.Args()...)
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("start k6: %w", err)
	}
	if c.OnStart != nil {
		c.OnStart()
	}
	err := cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, fmt.Errorf("wait k6: %w", err)
	}
	return 0, nil
}

// SummaryExport is the subset of k6's --summary-export file mcpload uses.
type SummaryExport struct {
	Metrics map[string]SummaryMetric `json:"metrics"`
}

// SummaryMetric holds one metric (or submetric) entry of the summary export.
// In this legacy format a threshold's boolean is true when the threshold FAILED.
type SummaryMetric struct {
	Thresholds map[string]bool `json:"thresholds"`
}

// ReadSummary reads a --summary-export file.
func ReadSummary(path string) (*SummaryExport, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseSummary(b)
}

// ParseSummary parses --summary-export JSON.
func ParseSummary(b []byte) (*SummaryExport, error) {
	var s SummaryExport
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse k6 summary export: %w", err)
	}
	return &s, nil
}
