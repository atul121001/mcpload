package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/analysis"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/k6run"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/sampler"
)

// multiFlag is a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

type runOpts struct {
	scenario, url, protocol, k6     string
	samplerKind, container, promURL string
	interval                        time.Duration
	soakMin, warmupMin, cooldownMin float64
	vus, minAgents                  int
	duration                        string
	env                             multiFlag
	label, gitSHA, gitRef           string
	out, html                       string
	uploadURL, key                  string
	leakSlope, minR2                float64
	includePayloads                 bool
	k6Out                           string
	waitReady                       time.Duration
	chaosRestart                    time.Duration
	chaosContainer, callsURL        string
	baseline                        string
	cmp                             analysis.CompareConfig
	failOnRegression                bool
	set                             map[string]bool
}

const runSynopsis = "mcpload run --url <mcp url> [--scenario <file.js|name>] [flags]"

func runFlags(o *runOpts, stderr io.Writer) *flag.FlagSet {
	fs := newFlagSet("run", runSynopsis, stderr)
	fs.StringVar(&o.scenario, "scenario", "", "scenario script (e.g. scenarios/soak.js) or bundled scenario name (e.g. soak, lb-check), looked up as scenarios/<name>.js in the current folder, then next to mcpload, then among the scenarios built into mcpload (default agent-session)")
	fs.StringVar(&o.url, "url", "", "MCP endpoint URL, passed to the scenario as MCP_URL (required)")
	fs.StringVar(&o.protocol, "protocol", "auto", "MCP protocol version or 'auto' (MCP_PROTOCOL)")
	// Advanced, hidden from -h: run a different engine (a k6 built with
	// xk6-mcpload) instead of the one embedded in mcpload. Also $MCPLOAD_ENGINE.
	fs.StringVar(&o.k6, "engine", "", "engine binary: a k6 built with xk6-mcpload (default: the engine embedded in mcpload)")
	fs.StringVar(&o.k6, "k6", "", "same as --engine")
	hideFlags(fs, "engine", "k6")
	fs.StringVar(&o.samplerKind, "sampler", "none", "server sampler: none | docker | prometheus")
	fs.StringVar(&o.container, "container", "", "container name or id for --sampler docker")
	fs.StringVar(&o.promURL, "prom-url", "", "Prometheus text endpoint (e.g. http://host:3001/metrics) for --sampler prometheus")
	fs.DurationVar(&o.interval, "interval", 10*time.Second, "sampling and series bucket interval")
	fs.Float64Var(&o.soakMin, "soak-min", 30, "soak: constant-load minutes (SOAK_MIN)")
	fs.Float64Var(&o.warmupMin, "warmup-min", 0, "soak: warm-up minutes (WARMUP_MIN; default 10% of soak, min 1)")
	fs.Float64Var(&o.cooldownMin, "cooldown-min", 5, "soak: cool-down minutes (COOLDOWN_MIN)")
	fs.IntVar(&o.vus, "vus", 0, "VUs, passed to the script as VUS (a scenario knob)")
	fs.StringVar(&o.duration, "duration", "", "duration, passed to the script as DURATION (a scenario knob)")
	fs.IntVar(&o.minAgents, "min-agents", 0, "step-load: concurrency the server must hold within budgets for the capacity verdict to pass (MIN_AGENTS)")
	fs.Var(&o.env, "env", "extra K=V setting passed to the scenario (repeatable)")
	fs.StringVar(&o.label, "label", "", "short display name of the target")
	fs.StringVar(&o.gitSHA, "git-sha", "", "git sha of the system under test (default $GITHUB_SHA)")
	fs.StringVar(&o.gitRef, "git-ref", "", "git ref of the system under test (default $GITHUB_REF)")
	fs.StringVar(&o.out, "out", "report.json", "report.json output path")
	fs.StringVar(&o.html, "html", "", "also write the HTML report to this path")
	fs.StringVar(&o.uploadURL, "upload-url", "", "base URL of a server that accepts report uploads (POST {url}/api/v1/runs)")
	fs.StringVar(&o.key, "key", "", "API key for the --upload-url server (default $MCPLOAD_KEY)")
	fs.Float64Var(&o.leakSlope, "leak-slope-mb-per-min", 1, "memory_leak RSS slope limit in MiB/min (1 MiB = 1048576 bytes)")
	fs.Float64Var(&o.minR2, "min-r2", 0.7, "minimum R² for a leak slope to count")
	fs.BoolVar(&o.includePayloads, "include-payloads", false, "keep tool arguments/results (INCLUDE_PAYLOADS=1)")
	fs.StringVar(&o.k6Out, "k6-out", "", "keep the engine's raw outputs (metrics.ndjson, summary.json) in this directory")
	fs.DurationVar(&o.waitReady, "wait-ready", 0, "before starting, wait up to this long (e.g. 2m) for the server to answer an MCP handshake (0 = don't wait)")
	fs.DurationVar(&o.chaosRestart, "chaos-restart", 0, "run `docker restart` on --chaos-container this long after the load starts (e.g. 30s); only on servers you own (0 = off)")
	fs.StringVar(&o.chaosContainer, "chaos-container", "", "container to restart for --chaos-restart (default: --container)")
	fs.StringVar(&o.baseline, "baseline", "", "compare with this baseline report.json (path or http(s) URL) after the run: adds the regression verdict and report.json comparison")
	compareFlags(fs, &o.cmp)
	fs.BoolVar(&o.failOnRegression, "fail-on-regression", true, "with --baseline: a regression fails the run (false: the regression verdict only warns)")
	fs.StringVar(&o.callsURL, "calls-url", "", "server endpoint listing executed call ids (GET ?prefix=, answers {\"executions\": {id: count}}), e.g. http://localhost:3019/calls, for the call_integrity verdict")
	return fs
}

func runCmd(args []string, stdout, stderr io.Writer) int {
	o := &runOpts{set: map[string]bool{}, cmp: analysis.DefaultCompareConfig()}
	fs := runFlags(o, stderr)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagExit(err)
	}
	fs.Visit(func(f *flag.Flag) { o.set[f.Name] = true })
	if err := o.validate(pos); err != nil {
		fmt.Fprintf(stderr, "mcpload run: %v\n\n", err)
		fs.Usage()
		return ExitError
	}
	code, err := execute(o, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "mcpload run: %v\n", err)
		return ExitError
	}
	return code
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

func (o *runOpts) validate(pos []string) error {
	if len(pos) > 0 {
		return fmt.Errorf("unexpected arguments: %v", pos)
	}
	if o.url == "" {
		return errors.New("--url is required")
	}
	cwd, _ := os.Getwd()
	scenario, err := resolveScenario(o.scenario, cwd, exeDir())
	if err != nil {
		return err
	}
	o.scenario = scenario
	switch o.samplerKind {
	case "none":
	case "docker":
		if o.container == "" {
			return errors.New("--sampler docker needs --container")
		}
	case "prometheus":
		if o.promURL == "" {
			return errors.New("--sampler prometheus needs --prom-url")
		}
	default:
		return fmt.Errorf("--sampler must be none, docker or prometheus (got %q)", o.samplerKind)
	}
	if o.waitReady < 0 {
		return errors.New("--wait-ready must not be negative")
	}
	if o.interval < time.Second {
		return errors.New("--interval must be at least 1s")
	}
	if o.chaosRestart < 0 {
		return errors.New("--chaos-restart must not be negative")
	}
	if o.chaosRestart > 0 && o.chaosContainer == "" {
		// Never restart something that was not named: only an explicit container.
		if o.container == "" {
			return errors.New("--chaos-restart needs --chaos-container (or --container)")
		}
		o.chaosContainer = o.container
	}
	if o.chaosContainer != "" && o.chaosRestart == 0 {
		return errors.New("--chaos-container needs --chaos-restart")
	}
	if o.callsURL != "" {
		if u, err := url.Parse(o.callsURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("--calls-url %q must be an http(s) URL", o.callsURL)
		}
	}
	for _, kv := range o.env {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" {
			return fmt.Errorf("--env %q must be KEY=VALUE", kv)
		}
	}
	if o.gitSHA == "" {
		o.gitSHA = os.Getenv("GITHUB_SHA")
	}
	if o.gitRef == "" {
		o.gitRef = os.Getenv("GITHUB_REF")
	}
	o.gitSHA = strings.ToLower(strings.TrimSpace(o.gitSHA))
	if o.gitSHA != "" && !shaRe.MatchString(o.gitSHA) {
		return fmt.Errorf("--git-sha %q must be 7-64 hex characters", o.gitSHA)
	}
	if o.uploadURL != "" && o.key == "" {
		o.key = os.Getenv("MCPLOAD_KEY")
		if o.key == "" {
			return errors.New("--upload-url needs --key or $MCPLOAD_KEY")
		}
	}
	if o.minAgents < 0 {
		return errors.New("--min-agents must not be negative")
	}
	if o.leakSlope <= 0 || o.minR2 < 0 || o.minR2 > 1 {
		return errors.New("--leak-slope-mb-per-min must be > 0 and --min-r2 in [0,1]")
	}
	if err := checkCompareConfig(o.cmp); err != nil {
		return err
	}
	return nil
}

// k6Env resolves the env passed to k6: --env first, explicit flags win.
// It returns the ordered KEY=VALUE list and a lookup map.
func (o *runOpts) k6Env() ([]string, map[string]string) {
	m := map[string]string{}
	var order []string
	put := func(k, v string) {
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = v
	}
	for _, kv := range o.env {
		k, v, _ := strings.Cut(kv, "=")
		put(k, v)
	}
	put("MCP_URL", o.url)
	if o.set["protocol"] || m["MCP_PROTOCOL"] == "" {
		put("MCP_PROTOCOL", o.protocol)
	}
	num := func(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
	if o.set["soak-min"] {
		put("SOAK_MIN", num(o.soakMin))
	}
	if o.set["warmup-min"] {
		put("WARMUP_MIN", num(o.warmupMin))
	}
	if o.set["cooldown-min"] {
		put("COOLDOWN_MIN", num(o.cooldownMin))
	}
	if o.set["vus"] {
		put("VUS", strconv.Itoa(o.vus))
	}
	if o.set["duration"] {
		put("DURATION", o.duration)
	}
	if o.set["min-agents"] {
		put("MIN_AGENTS", strconv.Itoa(o.minAgents))
	}
	if o.includePayloads {
		put("INCLUDE_PAYLOADS", "1")
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+m[k])
	}
	return out, m
}

// scriptEnv is the __ENV the k6 script sees: `k6 run` passes the OS
// environment (--include-system-env-vars defaults to true) and the -e values
// override it. soakPhases must read the same values or report.phases drift
// from what k6 actually ran.
func scriptEnv(osEnv []string, explicit map[string]string) map[string]string {
	m := make(map[string]string, len(osEnv)+len(explicit))
	for _, kv := range osEnv {
		// Windows has per-drive entries like "=C:=C:\dir"; skip empty keys.
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			m[k] = v
		}
	}
	for k, v := range explicit {
		m[k] = v
	}
	return m
}

// soakPhases mirrors scenarios/soak.js: SOAK_MIN (30), WARMUP_MIN (10% of
// SOAK_MIN, min 1), COOLDOWN_MIN (5); each rounded to whole seconds.
func soakPhases(env map[string]string) (report.Phases, error) {
	get := func(k string, def float64) (float64, error) {
		v, ok := env[k]
		if !ok || v == "" {
			return def, nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, fmt.Errorf("%s must be a number, got %q", k, v)
		}
		return f, nil
	}
	soak, err := get("SOAK_MIN", 30)
	if err != nil {
		return report.Phases{}, err
	}
	warm, err := get("WARMUP_MIN", math.Max(1, soak*0.1))
	if err != nil {
		return report.Phases{}, err
	}
	cool, err := get("COOLDOWN_MIN", 5)
	if err != nil {
		return report.Phases{}, err
	}
	w := math.Max(0, math.Round(warm*60))
	l := math.Max(0, math.Round(soak*60))
	c := math.Max(0, math.Round(cool*60))
	return report.Phases{WarmupEndS: w, LoadEndS: w + l, CooldownEndS: w + l + c}, nil
}

func newSampler(o *runOpts) sampler.Sampler {
	switch o.samplerKind {
	case "docker":
		return sampler.NewDocker(o.container)
	case "prometheus":
		return sampler.NewPrometheus(o.promURL, sampler.DefaultPromNames())
	}
	return sampler.NewNone()
}

func newRunID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func execute(o *runOpts, stdout, stderr io.Writer) (int, error) {
	logf := func(format string, a ...any) { fmt.Fprintf(stderr, "mcpload: "+format+"\n", a...) }
	logf("using scenario %s", o.scenario)
	// Read the baseline first, so a bad --baseline fails before the run, not after it.
	var baseline *report.Report
	if o.baseline != "" {
		var err error
		if baseline, err = loadReport(o.baseline); err != nil {
			return 0, fmt.Errorf("--baseline: %w", err)
		}
		logf("baseline: %s (run %s, %s)", o.baseline, baseline.Run.ID, describeRun(baseline))
	}

	bin, err := k6run.FindBinary(o.k6)
	if err != nil {
		return 0, err
	}
	ctx := context.Background()
	k6Version, err := k6run.Version(ctx, bin)
	if err != nil {
		return 0, err
	}
	env, explicitEnv := o.k6Env()
	envMap := scriptEnv(os.Environ(), explicitEnv)

	opts, err := k6run.Inspect(ctx, bin, o.scenario, env)
	if err != nil {
		logf("warning: %v (continuing without script options)", err)
		opts = nil
	}
	scenario := opts.ScenarioName()
	if scenario == "" {
		scenario = strings.TrimSuffix(filepath.Base(o.scenario), filepath.Ext(o.scenario))
	}
	soak := scenario == "soak"
	var soakPh report.Phases
	if soak {
		if soakPh, err = soakPhases(envMap); err != nil {
			return 0, err
		}
	}
	longLived := scenario == longLivedScenario
	if longLived {
		if soakPh, err = longLivedPhases(envMap); err != nil {
			return 0, err
		}
	}
	stepLoad := scenario == stepLoadScenario
	var capCfg analysis.CapacityConfig
	if stepLoad {
		if capCfg, err = capacityConfig(envMap); err != nil {
			return 0, err
		}
	}
	var recCfg analysis.RecoveryConfig
	if o.chaosRestart > 0 {
		if recCfg, err = recoveryConfig(envMap); err != nil {
			return 0, err
		}
		logf("chaos: will run `docker restart %s` %s after k6 starts. Only do this to servers you own.", o.chaosContainer, o.chaosRestart)
	}
	runID := newRunID()
	// Call ids ("<prefix>-<vu>-<n>") get a per-run prefix, so the server's record
	// of executed ids (--calls-url) can be narrowed to this run.
	callPrefix := envMap["CALL_ID_PREFIX"]
	if callPrefix == "" && (scenario == reconnectStormScenario || o.callsURL != "") {
		callPrefix = strings.ReplaceAll(runID, "-", "")[:8]
		env = append(env, "CALL_ID_PREFIX="+callPrefix)
	}

	if o.waitReady > 0 {
		probe, err := newReadyProbe(envMap)
		if err != nil {
			return 0, err
		}
		if probe.OAuth {
			logf("--wait-ready: the readiness probe does not fetch an OAuth token; a 401/403 answer counts as ready")
		}
		// Ctrl-C while waiting stops mcpload before k6 or the sampler start.
		wctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		err = waitReady(wctx, probe, readyWait{Max: o.waitReady}, logf)
		stop()
		if err != nil {
			return 0, err
		}
	}

	// Server sampler: fail fast on misconfiguration with one probe sample.
	smp := newSampler(o)
	if o.samplerKind != "none" {
		pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		_, perr := smp.Sample(pctx)
		cancel()
		if perr != nil {
			return 0, fmt.Errorf("%s sampler probe failed: %w", o.samplerKind, perr)
		}
	}
	// Server-side cancellation counters before the run (cancellation verdict).
	var cancelStart *sampler.CancelSnapshot
	if o.samplerKind == "prometheus" {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if snap, err := sampler.ScrapeCancel(pctx, o.promURL); err == nil && snap.Present {
			cancelStart = &snap
		}
		cancel()
	}

	dir := o.k6Out
	if dir == "" {
		if dir, err = os.MkdirTemp("", "mcpload-*"); err != nil {
			return 0, err
		}
		defer os.RemoveAll(dir)
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	ndjson := filepath.Join(dir, "metrics.ndjson")
	summaryPath := filepath.Join(dir, "summary.json")

	sctx, stopSampler := context.WithCancel(ctx)
	var points []k6run.ServerPoint
	var sampleErrs int
	var wg sync.WaitGroup
	ch := sampler.Collect(sctx, smp, o.interval, func(err error) {
		sampleErrs++
		if sampleErrs <= 5 {
			logf("warning: %s sample failed: %v", smp.Kind(), err)
		}
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for p := range ch {
			points = append(points, k6run.ServerPoint{T: p.T, RSSBytes: p.RSSBytes, HeapBytes: p.HeapBytes, OpenFDs: p.OpenFDs, ActiveSessions: p.ActiveSessions})
		}
	}()

	// k6 runs in its own process group; mcpload forwards SIGINT/SIGTERM
	// (Ctrl-C) once so k6 stops gracefully, kills it after a grace period or a
	// second signal, and still writes the report from the data so far.
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	var cpuMon *sampler.CPUMonitor
	var stopChaos func() chaosRun
	logf("engine k6 %s, scenario %s, target %s, sampler %s every %s", k6Version, o.scenario, o.url, smp.Kind(), o.interval)
	res, err := k6run.Run(k6run.RunConfig{
		Bin: bin, Script: o.scenario, Env: env,
		NDJSONPath: ndjson, SummaryPath: summaryPath,
		Stdout: stdout, Stderr: stderr,
		OnStart: func(pid int) {
			cpuMon = sampler.NewCPUMonitor(pid, o.interval)
			cpuMon.Start()
			if o.chaosRestart > 0 {
				stopChaos = scheduleRestart(o.chaosRestart, o.chaosContainer, execRunner, logf)
			}
		},
		Signals: sig,
		OnSignal: func(s os.Signal, forced bool) {
			switch {
			case s == nil:
				logf("k6 did not stop within %s; killing it", k6run.DefaultGrace)
			case forced:
				logf("second %v: killing k6", s)
			default:
				logf("%v: stopping k6 (up to %s; repeat to kill), then writing the report", s, k6run.DefaultGrace)
			}
		},
	})
	end := time.Now()
	var chaosRes chaosRun
	if stopChaos != nil {
		chaosRes = stopChaos()
		if !chaosRes.Ran {
			logf("warning: chaos: k6 finished before the restart of %s was due (%s); nothing was restarted", o.chaosContainer, o.chaosRestart)
		}
	}
	stopSampler()
	wg.Wait()
	var gen *report.Generator
	var cpuWindows []sampler.CPUWindow
	if cpuMon != nil {
		cs := cpuMon.Stop(res.Started, res.CPU, res.Ended)
		cpuWindows = cs.Windows
		gen = &report.Generator{Cores: cs.Cores}
		if cs.Avg != nil {
			gen.CPUAvgPct = report.F(round3(*cs.Avg))
		}
		if cs.Max != nil {
			gen.CPUMaxPct = report.F(round3(*cs.Max))
		}
		if cs.Avg == nil && cs.Errors > 0 {
			logf("warning: could not sample k6 CPU usage")
		}
	}
	if err != nil {
		return 0, err
	}
	code := res.ExitCode
	if res.Interrupted {
		logf("k6 was interrupted (exit code %d); writing the report from the data collected so far", code)
	}
	// step-load stops itself (exec.test.abort, ABORT_ERR_RATE) once a step has
	// clearly broken the server: an expected outcome, judged by the capacity verdict.
	stoppedEarly := stepLoad && code == k6run.ExitScriptAborted
	if stoppedEarly {
		logf("step-load stopped k6 early (exit code %d): a step broke the ABORT_ERR_RATE guard", code)
	}
	k6Failed := code != k6run.ExitOK && code != k6run.ExitThresholdsFailed && !stoppedEarly
	if k6Failed {
		logf("warning: k6 exited with code %d", code)
	}

	sum, err := k6run.ReadSummary(summaryPath)
	if err != nil {
		logf("warning: k6 summary export unavailable (%v); thresholds evaluated from raw samples", err)
		sum = nil
	}
	var defs []k6run.ThresholdDef
	if opts != nil {
		defs = opts.Thresholds
	}
	var track []string
	for _, d := range defs {
		track = append(track, d.Metric)
	}
	if sum != nil {
		for m, sm := range sum.Metrics {
			if len(sm.Thresholds) > 0 {
				track = append(track, m)
			}
		}
	}
	agg, err := k6run.ParseFile(ndjson, o.interval, track)
	if err != nil {
		return 0, fmt.Errorf("read k6 metrics (k6 exit code %d): %w", code, err)
	}

	// Run start = earliest k6 sample (the scenario clock), so phases line up.
	origin := agg.First()
	if end.Before(agg.Last()) {
		end = agg.Last()
	}
	durationS := end.Sub(origin).Seconds()
	// A trailing bucket covering < 50% of the interval is dropped from the
	// series (its rps would show a false drop and skew the fits).
	n, trimmed := k6run.KeepBuckets(durationS, o.interval)

	phases := report.Phases{WarmupEndS: 0, LoadEndS: round3(durationS), CooldownEndS: round3(durationS)}
	if soak || longLived {
		phases = soakPh
	}

	protocol := agg.Protocol()
	if protocol == "" && o.protocol != "auto" {
		protocol = o.protocol
	}
	if protocol == "" {
		protocol = "unknown"
	}

	r := &report.Report{
		SchemaVersion: report.SchemaVersion,
		Tool:          report.ToolInfo{Name: "mcpload", Version: Version},
		Run: report.Run{
			ID:        runID,
			StartedAt: report.FormatTime(origin),
			EndedAt:   report.FormatTime(end),
			DurationS: round3(durationS),
			Scenario:  scenario,
			Protocol:  protocol,
			Target:    report.Target{URL: o.url, Label: o.label},
			K6Version: k6Version,
			Load:      toLoad(opts.LoadShape()),
			Generator: gen,
		},
		Phases:           phases,
		PayloadsIncluded: o.includePayloads,
	}
	if o.gitSHA != "" {
		r.Run.Git = &report.Git{SHA: o.gitSHA, Ref: o.gitRef}
	}

	s := agg.Summary()
	r.Summary = report.Summary{Reqs: s.Reqs, Errors: s.Errors, ErrorRate: s.ErrorRate, ByErrorType: s.ByErrorType,
		Iterations: report.I64(s.Iterations), DroppedIterations: report.I64(s.DroppedIterations), ToolErrors: report.I64(s.ToolErrors)}
	if s.DroppedIterations > 0 {
		logf("warning: k6 dropped %d iterations (%d ran): the requested load was not reached", s.DroppedIterations, s.Iterations)
	}
	for _, t := range agg.Tools() {
		r.Tools = append(r.Tools, report.ToolStats{Name: t.Name, Reqs: t.Reqs, Errors: t.Errors, ErrorRate: t.ErrorRate,
			P50: round3(t.P50), P95: round3(t.P95), P99: round3(t.P99), Max: round3(t.Max)})
	}
	if wf := agg.Workflow(); wf != nil {
		r.Workflow = &report.Workflow{Runs: wf.Runs, Completed: wf.Completed, CompletionRate: round6(ratio(wf.Completed, wf.Runs)),
			DurationMs: toLatency(wf.Duration), Steps: []report.WorkflowStep{}}
		for _, st := range wf.Steps {
			r.Workflow.Steps = append(r.Workflow.Steps, report.WorkflowStep{Name: st.Name, Latency: toLatency(st.Latency)})
		}
	}
	for _, th := range agg.Thresholds(defs, sum, durationS) {
		var obs *float64
		if th.Observed != nil {
			obs = report.F(round6(*th.Observed))
		}
		r.Thresholds = append(r.Thresholds, report.Threshold{Metric: th.Metric, Expr: th.Expr, Passed: th.Passed, Observed: obs})
	}

	cs := agg.Series(n, !trimmed)
	r.Series = report.Series{
		IntervalS: o.interval.Seconds(),
		T:         k6run.Times(n, o.interval),
		Client: report.ClientSeries{P95Ms: roundSeries(cs.P95Ms, 3), ErrorRate: roundSeries(cs.ErrorRate, 6), RPS: roundSeries(cs.RPS, 3),
			DroppedIterations: cs.DroppedIterations},
		Server: report.ServerSeries{Sampler: smp.Kind(), RSSBytes: []*float64{}},
	}
	if len(cs.Tools) > 0 {
		r.Series.Tools = make(map[string]report.ToolSeries, len(cs.Tools))
		for name, p95 := range cs.Tools {
			r.Series.Tools[name] = report.ToolSeries{P95Ms: roundSeries(p95, 3)}
		}
	}
	if smp.Kind() != "none" {
		ss := k6run.AlignServerN(points, origin, o.interval, n, !trimmed)
		r.Series.Server.RSSBytes = ss.RSSBytes
		if smp.Kind() == "prometheus" {
			fill := func(v []*float64) []*float64 {
				if v == nil {
					return make([]*float64, n)
				}
				return v
			}
			r.Series.Server.HeapBytes = fill(ss.HeapBytes)
			r.Series.Server.OpenFDs = fill(ss.OpenFDs)
			r.Series.Server.ActiveSessions = fill(ss.ActiveSessions)
		} else {
			r.Series.Server.HeapBytes = ss.HeapBytes
			r.Series.Server.OpenFDs = ss.OpenFDs
			r.Series.Server.ActiveSessions = ss.ActiveSessions
		}
		if len(points) == 0 {
			logf("warning: the %s sampler produced no samples", smp.Kind())
		}
	}

	cfg := analysis.DefaultConfig()
	cfg.LeakSlopeMBPerMin = o.leakSlope
	cfg.MinR2 = o.minR2
	r.Verdicts = analysis.Verdicts(r, cfg)
	if o.chaosRestart > 0 {
		r.Chaos = chaosReport(o.chaosContainer, chaosRes, origin, agg, recCfg)
	}
	snf := s.ByErrorType["session_not_found"]
	var snfOutage int64
	if r.Chaos != nil && r.Chaos.Recovery != nil {
		// Sessions lost to the restart are expected; recovery judges them.
		snfOutage = min(snf, r.Chaos.Recovery.ErrorsByType["session_not_found"])
	}
	snfV := analysis.SessionNotFoundVerdict(float64(snf-snfOutage), float64(s.Reqs))
	if snfOutage > 0 {
		if snf == snfOutage {
			snfV.Message = "No 404 session-not-found responses outside the chaos restart."
		}
		snfV.Message += fmt.Sprintf(" %d came right after the restart, as expected (the server lost its sessions); the recovery verdict judges those.", snfOutage)
	}
	r.Verdicts = append(r.Verdicts, snfV,
		analysis.ThresholdVerdict(r.Thresholds),
		analysis.GeneratorVerdict(r))
	if r.Sessions = longSessions(agg); r.Sessions != nil || longLived {
		r.Verdicts = append(r.Verdicts, analysis.SessionSurvivalVerdict(r.Sessions))
	}
	if r.Chaos != nil || scenario == reconnectStormScenario {
		r.Verdicts = append(r.Verdicts, analysis.RecoveryVerdict(r.Chaos))
	}
	if tagged, attempts := agg.CallAttempts(); tagged > 0 || o.callsURL != "" {
		var fetchErr error
		if o.callsURL != "" {
			fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			execs, ferr := fetchExecutions(fctx, http.DefaultClient, o.callsURL, callPrefix+"-")
			cancel()
			if fetchErr = ferr; ferr != nil {
				logf("warning: could not read executed call ids from %s: %v", o.callsURL, ferr)
			} else {
				r.CallIntegrity = analysis.CallIntegrity(analysis.IntegrityInput{Source: o.callsURL, Tagged: tagged, Attempts: attempts, Executions: execs})
			}
		}
		ciV := analysis.CallIntegrityVerdict(tagged, r.CallIntegrity)
		if fetchErr != nil && tagged > 0 {
			ciV.Message = fmt.Sprintf("Skipped: could not read the server's executed call ids from %s: %v.", o.callsURL, fetchErr)
		}
		r.Verdicts = append(r.Verdicts, ciV)
	}
	if solo, mixed := agg.ScenarioTools(analysis.IsolationSoloScenario), agg.ScenarioTools(analysis.IsolationMixedScenario); solo != nil || mixed != nil {
		r.Verdicts = append(r.Verdicts, analysis.IsolationVerdict(phaseP95(solo), phaseP95(mixed)))
	}
	if sk := agg.Skew(); sk != nil || scenario == versionSkewScenario {
		r.Verdicts = append(r.Verdicts, analysis.VersionSkewVerdict(skewInput(sk)))
	}
	if steps := agg.Steps(); len(steps) > 0 || stepLoad {
		if !stepLoad {
			if capCfg, err = capacityConfig(envMap); err != nil {
				return 0, err
			}
		}
		capCfg.Planned = opts.StepLevels(stepLoadK6Scenario)
		capCfg.StoppedEarly = stoppedEarly
		cp, v := analysis.CapacityVerdict(buildSteps(steps, origin, cpuWindows), capCfg)
		if len(steps) > 0 {
			r.Capacity = cp
		}
		r.Verdicts = append(analysis.SkipDriftForSteps(r.Verdicts), v)
	}
	if cs := agg.Cancellations(); cs != nil {
		c := toCancellation(cs)
		var worst *analysis.CancelTool
		if cancelStart != nil && cs.Cancels > 0 {
			c.Server, worst = serverCancellation(o.promURL, *cancelStart, logf)
		}
		r.Cancellation = c
		r.Verdicts = append(r.Verdicts, analysis.CancellationVerdict(c, worst))
	}
	if baseline != nil {
		// Compare once every verdict is in: the comparison reads memory_leak and generator.
		r.Comparison = analysis.Compare(baseline, r, o.baseline, o.cmp, cfg)
		r.Verdicts = append(r.Verdicts, analysis.RegressionVerdict(r.Comparison, o.failOnRegression))
	}

	r.Normalize()
	checkErr := r.Check()
	if checkErr != nil {
		logf("error: report failed semantic checks:\n%v", checkErr)
	}
	if err := report.WriteJSON(o.out, r); err != nil {
		return 0, fmt.Errorf("write %s: %w", o.out, err)
	}
	logf("wrote %s", o.out)
	if o.html != "" {
		if err := writeHTML(o.html, r); err != nil {
			return 0, fmt.Errorf("write %s: %w", o.html, err)
		}
		logf("wrote %s", o.html)
	}

	printSteps(stderr, r.Capacity)
	if r.Comparison != nil {
		fmt.Fprintln(stderr)
		writeCompareText(stderr, r.Comparison, r)
	}
	printVerdicts(stderr, r)

	exit := ExitPass
	if !report.Passed(r) {
		exit = ExitFail
	}
	if o.uploadURL != "" && checkErr != nil {
		logf("error: not uploading: the report failed the semantic checks above (it was still written to %s)", o.out)
	} else if o.uploadURL != "" {
		body, err := report.Marshal(r)
		if err == nil {
			err = doUpload(o.uploadURL, o.key, body, stdout, stderr)
		}
		if err != nil {
			logf("error: upload failed: %v", err)
			return ExitError, nil
		}
	}
	if checkErr != nil {
		return ExitError, nil
	}
	if k6Failed {
		logf("error: k6 did not complete normally (exit code %d); the report may be incomplete", code)
		return ExitError, nil
	}
	return exit, nil
}

func printVerdicts(w io.Writer, r *report.Report) {
	fmt.Fprintf(w, "\nmcpload verdicts (%s, %s, %.0fs, %d reqs, %d errors):\n", r.Run.Scenario, r.Run.Protocol, r.Run.DurationS, r.Summary.Reqs, r.Summary.Errors)
	for _, v := range r.Verdicts {
		fmt.Fprintf(w, "  %-8s %-18s %s\n", strings.ToUpper(v.Status), v.ID, v.Message)
	}
	var failed []string
	for _, t := range r.Thresholds {
		if !t.Passed {
			failed = append(failed, t.Metric+" "+t.Expr)
		}
	}
	sort.Strings(failed)
	result := "PASS"
	if !report.Passed(r) {
		result = "FAIL"
	} else if report.HasWarnings(r) {
		result = "PASS (with warnings)"
	}
	fmt.Fprintf(w, "mcpload result: %s\n", result)
}

func toLoad(l k6run.Load) report.Load {
	return report.Load{Executor: l.Executor, VUs: l.VUs, MaxVUs: l.MaxVUs, ArrivalRate: l.ArrivalRate, ArrivalTimeUnitS: l.ArrivalTimeUnitS}
}

func toLatency(l k6run.Latency) report.Latency {
	return report.Latency{Count: l.Count, P50: round3(l.P50), P95: round3(l.P95), P99: round3(l.P99), Max: round3(l.Max)}
}

// ratio is a/b, or 0 when b is 0.
func ratio(a, b int64) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

func roundSeries(s []*float64, d int) []*float64 {
	p := math.Pow(10, float64(d))
	for i, v := range s {
		if v != nil {
			x := math.Round(*v*p) / p
			s[i] = &x
		}
	}
	return s
}

// versionSkewScenario is the options.tags.scenario_name of scenarios/version-skew.js.
const versionSkewScenario = "version-skew"

// skewInput converts the version-skew metrics for the version_skew verdict (nil: no requests).
func skewInput(s *k6run.SkewStats) analysis.SkewInput {
	if s == nil {
		return analysis.SkewInput{}
	}
	in := analysis.SkewInput{Requests: s.Requests, Failures: s.Failures, Replicas: s.Replicas,
		Negotiated: s.Negotiated, FailureMedianMs: map[string]float64{}}
	for k, l := range s.FailureMs {
		in.FailureMedianMs[k] = l.P50
	}
	return in
}

// phaseP95 converts one k6 scenario's per-tool latency for the tool_isolation verdict.
func phaseP95(m map[string]k6run.PhaseTool) map[string]analysis.PhaseP95 {
	out := make(map[string]analysis.PhaseP95, len(m))
	for n, t := range m {
		out[n] = analysis.PhaseP95{Calls: t.Calls, P95: t.P95}
	}
	return out
}
