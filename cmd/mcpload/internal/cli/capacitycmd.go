package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/k6run"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/sampler"
)

// The capacity subcommand is a front-end over `run --scenario step-load`: it
// turns --from/--to/--factor (or --steps) into STEPS, --step-duration into
// STEP_DURATION and --target into MIN_AGENTS, then runs the same code path
// (execute). --refine adds a second k6 run between the last passing and the
// first breaking step of the first pass.

const capacitySynopsis = "mcpload capacity --url <mcp url> [--from 10] [--to 1000] [--factor 2 | --steps 10,25,50] [--step-duration 1m] [--refine N] [--target N] [flags]"

// capacityRunFlags are the run flags the capacity subcommand shares (same
// variables, defaults and help text).
var capacityRunFlags = []string{"url", "protocol", "k6", "sampler", "container", "prom-url", "interval", "env",
	"label", "git-sha", "git-ref", "out", "html", "upload-url", "key", "include-payloads", "k6-out", "wait-ready"}

// stepEnvKeys are the step-load knobs the capacity flags own; --env must not set them.
var stepEnvKeys = []string{"STEPS", "START", "STEP_FACTOR", "MAX_VUS", "STEP_DURATION", "MIN_AGENTS"}

// Limits of the capacity flags.
const (
	maxCapacitySteps = 30
	maxRefine        = 10
)

type capacityOpts struct {
	from, to, refine, target int
	factor                   float64
	steps                    string
	stepDuration             time.Duration
}

func capacityFlags(o *runOpts, c *capacityOpts, stderr io.Writer) *flag.FlagSet {
	fs := newFlagSet("capacity", capacitySynopsis, stderr)
	fs.IntVar(&c.from, "from", 10, "first step, in agents (concurrent sessions)")
	fs.IntVar(&c.to, "to", 1000, "last step, in agents; the series is capped here")
	fs.Float64Var(&c.factor, "factor", 2, "each step is the previous one times this (geometric series from --from to --to)")
	fs.StringVar(&c.steps, "steps", "", "explicit comma-separated steps, e.g. 10,25,50,100 (instead of --from/--to/--factor)")
	fs.DurationVar(&c.stepDuration, "step-duration", time.Minute, "how long each step holds its level, after a 5s ramp (STEP_DURATION; RAMP via --env)")
	fs.IntVar(&c.refine, "refine", 0, "after the first pass, run this many extra steps between the last passing and the first breaking step in a second k6 run, to narrow the estimate (2 is a good choice)")
	fs.IntVar(&c.target, "target", 0, "agents the server must hold within budgets, else the run fails (MIN_AGENTS; 0 = report only)")
	run := runFlags(o, io.Discard)
	for _, name := range capacityRunFlags {
		f := run.Lookup(name)
		fs.Var(f.Value, f.Name, f.Usage)
	}
	return fs
}

func capacityCmd(args []string, stdout, stderr io.Writer) int {
	o := &runOpts{set: map[string]bool{}}
	c := &capacityOpts{}
	fs := capacityFlags(o, c, stderr)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagExit(err)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true; o.set[f.Name] = true })
	levels, err := c.apply(o, set)
	if err == nil {
		err = o.validate(pos)
	}
	if err != nil {
		fmt.Fprintf(stderr, "mcpload capacity: %v\n\n", err)
		fs.Usage()
		return ExitError
	}
	ramp := 5 * time.Second
	for _, kv := range o.env {
		if k, v, _ := strings.Cut(kv, "="); k == "RAMP" {
			if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
				ramp = d
			}
		}
	}
	total := time.Duration(len(levels)) * (ramp + c.stepDuration)
	fmt.Fprintf(stderr, "mcpload: capacity: steps %s agents, %s each after a %s ramp (about %s", joinInts(levels, ", "), c.stepDuration, ramp, total)
	if c.refine > 0 {
		fmt.Fprintf(stderr, ", then up to %d refinement steps (about %s)", c.refine, time.Duration(c.refine)*(ramp+c.stepDuration))
	}
	fmt.Fprintln(stderr, ")")
	code, err := execute(o, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "mcpload capacity: %v\n", err)
		return ExitError
	}
	return code
}

// apply checks the capacity flags and turns them into the step-load run
// they stand for; it returns the step levels.
func (c *capacityOpts) apply(o *runOpts, set map[string]bool) ([]int, error) {
	var levels []int
	if c.steps != "" {
		if set["from"] || set["to"] || set["factor"] {
			return nil, errors.New("--steps replaces --from/--to/--factor; use one or the other")
		}
		var err error
		if levels, err = parseSteps(c.steps); err != nil {
			return nil, err
		}
	} else {
		if c.from < 1 || c.to < c.from {
			return nil, fmt.Errorf("--from must be >= 1 and --to >= --from (got %d and %d)", c.from, c.to)
		}
		if !(c.factor > 1) || math.IsInf(c.factor, 0) {
			return nil, fmt.Errorf("--factor must be > 1 (got %v)", c.factor)
		}
		levels = stepSeries(c.from, c.to, c.factor)
	}
	if len(levels) > maxCapacitySteps {
		return nil, fmt.Errorf("%d steps is too many (max %d): raise --factor or narrow --from/--to", len(levels), maxCapacitySteps)
	}
	if c.stepDuration < time.Second {
		return nil, errors.New("--step-duration must be at least 1s")
	}
	if c.refine < 0 || c.refine > maxRefine {
		return nil, fmt.Errorf("--refine must be between 0 and %d", maxRefine)
	}
	if c.target < 0 {
		return nil, errors.New("--target must not be negative")
	}
	for _, kv := range o.env {
		if k, _, _ := strings.Cut(kv, "="); slices.Contains(stepEnvKeys, k) {
			return nil, fmt.Errorf("--env %s: use the capacity flags (--from/--to/--factor/--steps, --step-duration, --target) instead", k)
		}
	}
	o.scenario = stepLoadScenario
	o.env = append(o.env, "STEPS="+joinInts(levels, ","),
		"STEP_DURATION="+strconv.FormatFloat(c.stepDuration.Seconds(), 'f', -1, 64)+"s")
	if c.target > 0 {
		o.minAgents, o.set["min-agents"] = c.target, true
	}
	o.refine = c.refine
	o.hints = capacityStepHints
	return levels, nil
}

// stepSeries is the geometric series from, from*factor, ... capped at to
// (each rounded, strictly increasing). to is always the last step: it is
// appended when the series stops short of it by more than sqrt(factor), and
// otherwise replaces the last level (so the top step is never a tiny one).
func stepSeries(from, to int, factor float64) []int {
	var out []int
	for v := float64(from); math.Round(v) <= float64(to); v *= factor {
		n := int(math.Round(v))
		if len(out) > 0 && n <= out[len(out)-1] {
			n = out[len(out)-1] + 1
		}
		if n > to {
			break
		}
		out = append(out, n)
	}
	last := out[len(out)-1]
	switch {
	case last == to:
	case float64(to)/float64(last) >= math.Sqrt(factor) || len(out) == 1:
		out = append(out, to)
	default:
		out[len(out)-1] = to
	}
	return out
}

// parseSteps parses --steps: strictly increasing positive integers.
func parseSteps(s string) ([]int, error) {
	var out []int
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("--steps must be comma-separated positive integers, got %q", f)
		}
		if len(out) > 0 && n <= out[len(out)-1] {
			return nil, fmt.Errorf("--steps must increase: %s", s)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, errors.New("--steps is empty")
	}
	return out, nil
}

// refineLevels are up to n levels evenly spaced strictly between the last
// passing and the first breaking step (n = 1 halves the gap). nil without
// such a bracket or when the result is inconclusive.
func refineLevels(c *report.Capacity, n int) []int {
	if n < 1 || c == nil || c.MaxSustainableVUs == nil || c.BreakingVUs == nil || c.Inconclusive {
		return nil
	}
	lo, hi := *c.MaxSustainableVUs, *c.BreakingVUs
	var out []int
	for k := 1; k <= n; k++ {
		v := lo + int(math.Round(float64(k)*float64(hi-lo)/float64(n+1)))
		if v > lo && v < hi && (len(out) == 0 || v > out[len(out)-1]) {
			out = append(out, v)
		}
	}
	return out
}

// refinePass runs step-load a second time with STEPS=levels and returns its
// steps (Refinement set), timed against the first pass's origin. Only the
// steps are kept: the report's series, totals and other verdicts cover the
// first pass.
func refinePass(o *runOpts, bin string, env []string, levels []int, origin time.Time, sig <-chan os.Signal,
	stdout, stderr io.Writer, logf func(string, ...any)) ([]report.Step, error) {
	var renv []string
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != "STEPS" && k != "START" && k != "STEP_FACTOR" && k != "MAX_VUS" {
			renv = append(renv, kv)
		}
	}
	renv = append(renv, "STEPS="+joinInts(levels, ","))
	dir := filepath.Join(o.k6Out, "refine")
	if o.k6Out == "" {
		var err error
		if dir, err = os.MkdirTemp("", "mcpload-refine-*"); err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ndjson := filepath.Join(dir, "metrics.ndjson")
	logf("refine: second k6 run with steps %s agents (between the last passing and the first breaking step)", joinInts(levels, ", "))
	var cpuMon *sampler.CPUMonitor
	res, err := k6run.Run(k6run.RunConfig{
		Bin: bin, Script: o.scenario, Env: renv,
		NDJSONPath: ndjson, SummaryPath: filepath.Join(dir, "summary.json"),
		Stdout: stdout, Stderr: stderr,
		OnStart: func(pid int) {
			cpuMon = sampler.NewCPUMonitor(pid, o.interval)
			cpuMon.Start()
		},
		Signals: sig,
	})
	var cpu []sampler.CPUWindow
	if cpuMon != nil {
		cpu = cpuMon.Stop(res.Started, res.CPU, res.Ended).Windows
	}
	if err != nil {
		return nil, err
	}
	switch res.ExitCode {
	case k6run.ExitOK, k6run.ExitThresholdsFailed:
	case k6run.ExitScriptAborted:
		logf("refine: step-load stopped the refinement run early (ABORT_ERR_RATE)")
	default:
		logf("warning: refine: k6 exited with code %d", res.ExitCode)
	}
	agg, err := k6run.ParseFile(ndjson, o.interval, nil)
	if err != nil {
		return nil, fmt.Errorf("read the refinement run's metrics: %w", err)
	}
	steps := buildSteps(agg.Steps(), origin, cpu)
	for i := range steps {
		steps[i].Refinement = true
	}
	return steps, nil
}

// mergeSteps merges the refinement steps into the first pass's, sorted by VUs
// (a refinement level never repeats a first-pass one; a clash keeps the first pass).
func mergeSteps(first, extra []report.Step) []report.Step {
	out := append([]report.Step(nil), first...)
	for _, s := range extra {
		if !slices.ContainsFunc(out, func(x report.Step) bool { return x.VUs == s.VUs }) {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b report.Step) int { return a.VUs - b.VUs })
	return out
}

func joinInts(v []int, sep string) string {
	s := make([]string, len(v))
	for i, n := range v {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, sep)
}
