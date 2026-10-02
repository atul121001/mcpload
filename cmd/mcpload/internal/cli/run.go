package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	vus                             int
	duration                        string
	env                             multiFlag
	label, gitSHA, gitRef           string
	out, html                       string
	uploadURL, key                  string
	leakSlope, minR2                float64
	includePayloads                 bool
	k6Out                           string
	set                             map[string]bool
}

const runSynopsis = "mcpload run --scenario <file.js> --url <mcp url> [flags]"

func runFlags(o *runOpts, stderr io.Writer) *flag.FlagSet {
	fs := newFlagSet("run", runSynopsis, stderr)
	fs.StringVar(&o.scenario, "scenario", "", "k6 scenario script, e.g. scenarios/soak.js (required)")
	fs.StringVar(&o.url, "url", "", "MCP endpoint URL, passed to k6 as MCP_URL (required)")
	fs.StringVar(&o.protocol, "protocol", "auto", "MCP protocol version or 'auto' (MCP_PROTOCOL)")
	fs.StringVar(&o.k6, "k6", "", "k6 binary built with xk6-mcpload (default ./k6.exe or ./k6, then k6 on PATH)")
	fs.StringVar(&o.samplerKind, "sampler", "none", "server sampler: none | docker | prometheus")
	fs.StringVar(&o.container, "container", "", "container name or id for --sampler docker")
	fs.StringVar(&o.promURL, "prom-url", "", "Prometheus text endpoint (e.g. http://host:3001/metrics) for --sampler prometheus")
	fs.DurationVar(&o.interval, "interval", 10*time.Second, "sampling and series bucket interval")
	fs.Float64Var(&o.soakMin, "soak-min", 30, "soak: constant-load minutes (SOAK_MIN)")
	fs.Float64Var(&o.warmupMin, "warmup-min", 0, "soak: warm-up minutes (WARMUP_MIN; default 10% of soak, min 1)")
	fs.Float64Var(&o.cooldownMin, "cooldown-min", 5, "soak: cool-down minutes (COOLDOWN_MIN)")
	fs.IntVar(&o.vus, "vus", 0, "VUs, passed to the script as VUS (scenario knob, not k6 --vus)")
	fs.StringVar(&o.duration, "duration", "", "duration, passed to the script as DURATION (scenario knob, not k6 --duration)")
	fs.Var(&o.env, "env", "extra K=V passed to k6 with -e (repeatable)")
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
	fs.StringVar(&o.k6Out, "k6-out", "", "keep k6's raw outputs (metrics.ndjson, summary.json) in this directory")
	return fs
}

func runCmd(args []string, stdout, stderr io.Writer) int {
	o := &runOpts{set: map[string]bool{}}
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
	if o.scenario == "" || o.url == "" {
		return errors.New("--scenario and --url are required")
	}
	if _, err := os.Stat(o.scenario); err != nil {
		return fmt.Errorf("scenario: %w", err)
	}
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
	if o.interval < time.Second {
		return errors.New("--interval must be at least 1s")
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
	if o.leakSlope <= 0 || o.minR2 < 0 || o.minR2 > 1 {
		return errors.New("--leak-slope-mb-per-min must be > 0 and --min-r2 in [0,1]")
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
	if o.includePayloads {
		put("INCLUDE_PAYLOADS", "1")
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+m[k])
	}
	return out, m
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

	bin, err := k6run.FindBinary(o.k6)
	if err != nil {
		return 0, err
	}
	ctx := context.Background()
	k6Version, err := k6run.Version(ctx, bin)
	if err != nil {
		return 0, err
	}
	env, envMap := o.k6Env()

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

	// k6 receives Ctrl-C itself (same console) and stops gracefully; mcpload
	// keeps running so it can still write the report.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		for range sig {
			logf("interrupt: waiting for k6 to stop, then writing the report")
		}
	}()

	logf("k6 %s, scenario %s, target %s, sampler %s every %s", k6Version, o.scenario, o.url, smp.Kind(), o.interval)
	code, err := k6run.Run(k6run.RunConfig{
		Bin: bin, Script: o.scenario, Env: env,
		NDJSONPath: ndjson, SummaryPath: summaryPath,
		Stdout: stdout, Stderr: stderr,
	})
	end := time.Now()
	stopSampler()
	wg.Wait()
	if err != nil {
		return 0, err
	}
	k6Failed := code != k6run.ExitOK && code != k6run.ExitThresholdsFailed
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
	n := int(math.Ceil(durationS / o.interval.Seconds()))
	if n < 1 {
		n = 1
	}

	phases := report.Phases{WarmupEndS: 0, LoadEndS: round3(durationS), CooldownEndS: round3(durationS)}
	if soak {
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
			ID:        newRunID(),
			StartedAt: report.FormatTime(origin),
			EndedAt:   report.FormatTime(end),
			DurationS: round3(durationS),
			Scenario:  scenario,
			Protocol:  protocol,
			Target:    report.Target{URL: o.url, Label: o.label},
			K6Version: k6Version,
			Load:      toLoad(opts.LoadShape()),
		},
		Phases:           phases,
		PayloadsIncluded: o.includePayloads,
	}
	if o.gitSHA != "" {
		r.Run.Git = &report.Git{SHA: o.gitSHA, Ref: o.gitRef}
	}

	s := agg.Summary()
	r.Summary = report.Summary{Reqs: s.Reqs, Errors: s.Errors, ErrorRate: s.ErrorRate, ByErrorType: s.ByErrorType}
	for _, t := range agg.Tools() {
		r.Tools = append(r.Tools, report.ToolStats{Name: t.Name, Reqs: t.Reqs, Errors: t.Errors, ErrorRate: t.ErrorRate,
			P50: round3(t.P50), P95: round3(t.P95), P99: round3(t.P99), Max: round3(t.Max)})
	}
	for _, th := range agg.Thresholds(defs, sum, durationS) {
		var obs *float64
		if th.Observed != nil {
			obs = report.F(round6(*th.Observed))
		}
		r.Thresholds = append(r.Thresholds, report.Threshold{Metric: th.Metric, Expr: th.Expr, Passed: th.Passed, Observed: obs})
	}

	cs := agg.Client(n)
	r.Series = report.Series{
		IntervalS: o.interval.Seconds(),
		T:         k6run.Times(n, o.interval),
		Client:    report.ClientSeries{P95Ms: roundSeries(cs.P95Ms, 3), ErrorRate: roundSeries(cs.ErrorRate, 6), RPS: roundSeries(cs.RPS, 3)},
		Server:    report.ServerSeries{Sampler: smp.Kind(), RSSBytes: []*float64{}},
	}
	if smp.Kind() != "none" {
		ss := k6run.AlignServer(points, origin, o.interval, n)
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
	r.Verdicts = append(r.Verdicts,
		analysis.SessionNotFoundVerdict(float64(s.ByErrorType["session_not_found"]), float64(s.Reqs)),
		analysis.ThresholdVerdict(r.Thresholds))

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

	printVerdicts(stderr, r)

	exit := ExitPass
	if !report.Passed(r) {
		exit = ExitFail
	}
	if o.uploadURL != "" {
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
