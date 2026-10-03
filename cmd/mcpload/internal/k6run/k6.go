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
	"time"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/engine"
)

// Engine sources reported by Locate.
const (
	SourceFlag     = "--engine"
	SourceEnv      = "MCPLOAD_ENGINE"
	SourceEmbedded = "embedded"
	SourceCwd      = "current folder"
	SourceExeDir   = "next to mcpload"
	SourcePath     = "PATH"
)

// FindBinary resolves the engine (k6 built with xk6-mcpload); see Locate.
func FindBinary(explicit string) (string, error) {
	p, _, err := Locate(explicit)
	return p, err
}

// Locate resolves the engine binary and says where it came from: an explicit
// path (--engine / --k6) wins, then $MCPLOAD_ENGINE, then the engine embedded
// in release builds. Dev builds without it look for ./k6.exe (Windows) or
// ./k6 in the working directory, then next to the real mcpload executable
// (older release archives shipped k6 there), then k6 on PATH.
func Locate(explicit string) (path, source string, err error) {
	if explicit != "" {
		p, err := explicitBinary(explicit)
		return p, SourceFlag, err
	}
	if env := os.Getenv("MCPLOAD_ENGINE"); env != "" {
		p, err := explicitBinary(env)
		if err != nil {
			err = fmt.Errorf("MCPLOAD_ENGINE: %w", err)
		}
		return p, SourceEnv, err
	}
	if engine.Embedded() {
		p, err := engine.Path()
		return p, SourceEmbedded, err
	}
	local := []string{"k6"}
	if runtime.GOOS == "windows" {
		local = []string{"k6.exe", "k6"}
	}
	for _, name := range local {
		if st, err := os.Stat(name); err == nil && !st.IsDir() {
			p, err := filepath.Abs(name)
			return p, SourceCwd, err
		}
	}
	if dir := ExeDir(); dir != "" {
		for _, name := range local {
			p := filepath.Join(dir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, SourceExeDir, nil
			}
		}
	}
	p, err := exec.LookPath("k6")
	if err != nil {
		return "", "", errors.New("the mcpload engine was not found: this build has no embedded engine, and no k6 built with xk6-mcpload is in the current folder, next to mcpload, or on PATH; install a release build (see the README) or pass --engine <path>")
	}
	return p, SourcePath, nil
}

func explicitBinary(p string) (string, error) {
	if lp, err := exec.LookPath(p); err == nil {
		return lp, nil
	}
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return filepath.Abs(p)
	}
	return "", fmt.Errorf("engine binary %q not found", p)
}

// ExeDir is the folder of the real mcpload executable, or "" if unknown.
// Symlinks are resolved, so an install that links bin/mcpload to
// ~/.mcpload/current/mcpload (install.sh, Homebrew) still finds the k6 and
// scenarios/ that ship next to the real binary.
func ExeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return RealDir(exe)
}

// RealDir returns the folder of path after resolving symlinks; if they cannot
// be resolved it falls back to the folder of path itself.
func RealDir(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		path = r
	}
	return filepath.Dir(path)
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

// ParseVersion extracts the version from `k6 version` output.
func ParseVersion(out string) string { return parseVersion(out) }

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

// Inspect runs `k6 inspect` on the script with the given env and parses the
// options. Like `k6 run` (whose --include-system-env-vars defaults to true),
// the script sees the OS environment, with the -e values winning.
func Inspect(ctx context.Context, bin, script string, env []string) (*Options, error) {
	args := append([]string{"inspect", "--include-system-env-vars"}, EnvArgs(env)...)
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
	// OnStart is called right after the process started, with its pid.
	OnStart func(pid int)
	// Signals, when set, are forwarded to k6 so it stops gracefully (on
	// Windows as CTRL_BREAK to k6's own process group). k6 is killed after
	// Grace (default DefaultGrace) or on a second signal.
	Signals <-chan os.Signal
	Grace   time.Duration
	// OnSignal, when set, is called for each received signal (for logging).
	OnSignal func(sig os.Signal, forced bool)
}

// DefaultGrace is how long k6 may take to stop after a forwarded signal.
const DefaultGrace = 30 * time.Second

// Result is the outcome of one `k6 run`.
type Result struct {
	ExitCode    int
	Interrupted bool          // a signal was forwarded to k6
	Killed      bool          // k6 was killed (grace expired or second signal)
	CPU         time.Duration // total user+system CPU time of the k6 process
	Started     time.Time
	Ended       time.Time
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
	// ExitScriptAborted is exec.test.abort() from the script; step-load uses
	// it to stop once a step has clearly broken the server.
	ExitScriptAborted = 108
)

// Run executes k6 and waits. It returns k6's exit code; err is set only when
// the process could not be started or waited for.
func Run(c RunConfig) (Result, error) {
	return runCmd(c.command(), c)
}

// command is the `k6 run` process: the OS environment (k6 passes it to the
// script) plus K6_NO_USAGE_REPORT (see EngineEnv).
func (c RunConfig) command() *exec.Cmd {
	cmd := exec.Command(c.Bin, c.Args()...)
	cmd.Env = EngineEnv(os.Environ())
	return cmd
}

// NoUsageReportEnv turns off the anonymous usage report that `k6 run` would
// otherwise POST to Grafana (stats.grafana.org) at the end of every run,
// together with a fetch of the k6 extension catalog from registry.k6.io.
// mcpload sends nothing to hosts the user didn't configure.
const NoUsageReportEnv = "K6_NO_USAGE_REPORT"

// EngineEnv returns env with NoUsageReportEnv=true added, unless env already
// sets it to a value (K6_NO_USAGE_REPORT=false opts back in to k6's usage
// report). exec.Cmd keeps the last of duplicate keys, so the added value wins
// over an empty one.
func EngineEnv(env []string) []string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && v != "" && strings.EqualFold(k, NoUsageReportEnv) {
			return env
		}
	}
	return append(env[:len(env):len(env)], NoUsageReportEnv+"=true")
}

// runCmd starts cmd in its own process group (so a terminal Ctrl-C reaches
// mcpload only, which forwards it once), forwards c.Signals and waits.
func runCmd(cmd *exec.Cmd, c RunConfig) (Result, error) {
	res := Result{ExitCode: -1}
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	cmd.Stdin = nil
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return res, fmt.Errorf("start k6: %w", err)
	}
	res.Started = time.Now()
	if c.OnStart != nil {
		c.OnStart(cmd.Process.Pid)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	grace := c.Grace
	if grace <= 0 {
		grace = DefaultGrace
	}
	var graceC <-chan time.Time
	var err error
wait:
	for {
		select {
		case err = <-done:
			break wait
		case sig := <-c.Signals:
			forced := res.Interrupted
			if c.OnSignal != nil {
				c.OnSignal(sig, forced)
			}
			if forced {
				res.Killed = true
				_ = cmd.Process.Kill()
				continue
			}
			res.Interrupted = true
			if ierr := interruptProcess(cmd.Process, sig); ierr != nil {
				res.Killed = true
				_ = cmd.Process.Kill()
				continue
			}
			t := time.NewTimer(grace)
			defer t.Stop()
			graceC = t.C
		case <-graceC:
			graceC = nil
			if c.OnSignal != nil {
				c.OnSignal(nil, true)
			}
			res.Killed = true
			_ = cmd.Process.Kill()
		}
	}
	res.Ended = time.Now()
	if ps := cmd.ProcessState; ps != nil {
		res.CPU = ps.UserTime() + ps.SystemTime()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("wait k6: %w", err)
	}
	res.ExitCode = 0
	return res, nil
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
