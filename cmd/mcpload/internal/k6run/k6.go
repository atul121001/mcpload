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
)

// FindBinary resolves the k6 binary: an explicit path wins; otherwise ./k6.exe
// (Windows) or ./k6 in the working directory, then next to the mcpload
// executable (release archives ship both together), then k6 on PATH.
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
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, name := range local {
			p := filepath.Join(dir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, nil
			}
		}
	}
	p, err := exec.LookPath("k6")
	if err != nil {
		return "", errors.New("k6 binary not found (looked in the current folder, next to mcpload, and on PATH); download a release or pass --k6")
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
)

// Run executes k6 and waits. It returns k6's exit code; err is set only when
// the process could not be started or waited for.
func Run(c RunConfig) (Result, error) {
	return runCmd(exec.Command(c.Bin, c.Args()...), c)
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
