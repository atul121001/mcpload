package cli

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

func TestStepSeries(t *testing.T) {
	cases := []struct {
		from, to int
		factor   float64
		want     string
	}{
		{10, 1000, 2, "[10 20 40 80 160 320 640 1000]"}, // 1000/640 > sqrt(2): appended
		{10, 700, 2, "[10 20 40 80 160 320 700]"},       // 700/640 is close: replaces 640
		{5, 80, 2, "[5 10 20 40 80]"},                   // lands on --to
		{10, 80, 2, "[10 20 40 80]"},
		{10, 10, 2, "[10]"},
		{10, 12, 2, "[10 12]"},               // a single level is never replaced
		{1, 5, 1.2, "[1 2 3 4 5]"},           // rounding never repeats a level
		{10, 1000, 3, "[10 30 90 270 1000]"}, // 1000/810 < sqrt(3): 810 replaced
		{25, 400, 1.5, "[25 38 56 84 127 190 285 400]"},
	}
	for _, tc := range cases {
		if got := fmt.Sprint(stepSeries(tc.from, tc.to, tc.factor)); got != tc.want {
			t.Errorf("stepSeries(%d, %d, %v) = %s, want %s", tc.from, tc.to, tc.factor, got, tc.want)
		}
	}
}

func TestParseSteps(t *testing.T) {
	if got, err := parseSteps(" 10, 25,50 ,100,"); err != nil || fmt.Sprint(got) != "[10 25 50 100]" {
		t.Errorf("got %v, %v", got, err)
	}
	for _, bad := range []string{"", ",", "10,10", "20,10", "0,5", "5,x", "2.5"} {
		if _, err := parseSteps(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

// parseCapacity parses capacity flags the way capacityCmd does, without running anything.
func parseCapacity(args ...string) (*runOpts, []int, error) {
	o := &runOpts{set: map[string]bool{}}
	c := &capacityOpts{}
	fs := capacityFlags(o, c, io.Discard)
	if _, err := parseInterspersed(fs, args); err != nil {
		return nil, nil, err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true; o.set[f.Name] = true })
	levels, err := c.apply(o, set)
	return o, levels, err
}

func TestCapacityFlags(t *testing.T) {
	o, levels, err := parseCapacity("--url", "http://x/mcp", "--from", "5", "--to", "80", "--step-duration", "20s",
		"--target", "40", "--refine", "2", "--env", "RAMP=3s", "--label", "demo", "--html", "r.html")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(levels) != "[5 10 20 40 80]" || o.scenario != stepLoadScenario || o.refine != 2 || o.hints != capacityStepHints {
		t.Errorf("levels %v, opts %+v", levels, o)
	}
	_, m := o.k6Env()
	if m["STEPS"] != "5,10,20,40,80" || m["STEP_DURATION"] != "20s" || m["MIN_AGENTS"] != "40" || m["RAMP"] != "3s" || m["MCP_URL"] != "http://x/mcp" {
		t.Errorf("k6 env = %v", m)
	}
	if o.label != "demo" || o.html != "r.html" || o.out != "report.json" || o.protocol != "auto" || o.interval != 10*time.Second {
		t.Errorf("shared run flags: %+v", o)
	}

	// Defaults: 10..1000 by 2, 1m steps, no target.
	o, levels, err = parseCapacity("--url", "http://x/mcp")
	if err != nil {
		t.Fatal(err)
	}
	_, m = o.k6Env()
	if fmt.Sprint(levels) != "[10 20 40 80 160 320 640 1000]" || m["STEP_DURATION"] != "60s" || m["MIN_AGENTS"] != "" || o.refine != 0 {
		t.Errorf("defaults: levels %v, env %v", levels, m)
	}

	o, levels, err = parseCapacity("--url", "http://x/mcp", "--steps", "10,25,50", "--step-duration", "1.5s")
	if err != nil {
		t.Fatal(err)
	}
	if _, m = o.k6Env(); fmt.Sprint(levels) != "[10 25 50]" || m["STEPS"] != "10,25,50" || m["STEP_DURATION"] != "1.5s" {
		t.Errorf("--steps: levels %v, env %v", levels, m)
	}

	for _, bad := range [][]string{
		{"--steps", "10,25", "--to", "100"},
		{"--steps", "25,10"},
		{"--from", "0"},
		{"--from", "100", "--to", "50"},
		{"--factor", "1"},
		{"--from", "1", "--to", "1000000", "--factor", "1.1"}, // too many steps
		{"--step-duration", "500ms"},
		{"--refine", "-1"},
		{"--refine", "11"},
		{"--target", "-5"},
		{"--env", "STEPS=1,2"},
		{"--env", "MIN_AGENTS=10"},
	} {
		if _, _, err := parseCapacity(append([]string{"--url", "http://x/mcp"}, bad...)...); err == nil {
			t.Errorf("%v: want an error", bad)
		}
	}
	// Run-only flags are not capacity flags.
	for _, f := range []string{"--scenario", "--vus", "--soak-min", "--min-agents", "--chaos-restart"} {
		if _, _, err := parseCapacity("--url", "http://x/mcp", f, "1"); err == nil {
			t.Errorf("%s should be rejected", f)
		}
	}
}

func TestCapacityCmdUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"--url", "http://x/mcp", "--steps", "5", "--from", "5"}, {"--url", "http://x/mcp", "extra"}} {
		var stdout, stderr bytes.Buffer
		if code := Main(append([]string{"capacity"}, args...), &stdout, &stderr); code != ExitError {
			t.Errorf("%v: exit %d, want %d", args, code, ExitError)
		}
		if !strings.Contains(stderr.String(), "Usage: mcpload capacity") {
			t.Errorf("%v: no usage:\n%s", args, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"capacity", "-h"}, &stdout, &stderr); code != ExitPass || !strings.Contains(stderr.String(), "-refine") || !strings.Contains(stderr.String(), "-sampler") {
		t.Errorf("-h: exit %d\n%s", code, stderr.String())
	}
}

func TestRefineLevels(t *testing.T) {
	cp := func(held, broke int, inconclusive bool) *report.Capacity {
		return &report.Capacity{MaxSustainableVUs: report.I(held), BreakingVUs: report.I(broke), Inconclusive: inconclusive}
	}
	cases := []struct {
		c    *report.Capacity
		n    int
		want string
	}{
		{cp(20, 40, false), 1, "[30]"},
		{cp(20, 40, false), 2, "[27 33]"},
		{cp(10, 20, false), 3, "[13 15 18]"},
		{cp(10, 12, false), 4, "[11]"}, // no more levels than integers in the gap
		{cp(10, 11, false), 2, "[]"},
		{cp(20, 40, true), 2, "[]"},                                  // inconclusive: refine the machine, not the steps
		{&report.Capacity{MaxSustainableVUs: report.I(80)}, 2, "[]"}, // nothing broke
		{&report.Capacity{BreakingVUs: report.I(5)}, 2, "[]"},        // first step broke
		{cp(20, 40, false), 0, "[]"},
	}
	for _, tc := range cases {
		if got := fmt.Sprint(refineLevels(tc.c, tc.n)); got != tc.want && !(tc.want == "[]" && got == "[]") {
			t.Errorf("refineLevels(%v..%v, %d) = %s, want %s", tc.c.MaxSustainableVUs, tc.c.BreakingVUs, tc.n, got, tc.want)
		}
	}
}

func TestMergeSteps(t *testing.T) {
	first := []report.Step{{VUs: 10}, {VUs: 20}, {VUs: 40}}
	extra := []report.Step{{VUs: 27, Refinement: true}, {VUs: 33, Refinement: true}, {VUs: 40, Refinement: true}}
	got := mergeSteps(first, extra)
	var vus []int
	for _, s := range got {
		vus = append(vus, s.VUs)
	}
	if !slices.Equal(vus, []int{10, 20, 27, 33, 40}) || !got[2].Refinement || got[4].Refinement {
		t.Errorf("merged = %+v", got)
	}
}
