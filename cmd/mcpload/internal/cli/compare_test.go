package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/analysis"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

var examples = filepath.Join("..", "..", "..", "..", "report", "examples")

// comparePair writes the healthy example as the baseline and a changed copy
// as the current run: `search` p95 372 -> 500 ms, `big` removed, `new` added,
// `flaky` with 30 calls.
func comparePair(t *testing.T) (base, cur string) {
	t.Helper()
	dir := t.TempDir()
	r, err := report.ReadJSON(filepath.Join(examples, "healthy.json"))
	if err != nil {
		t.Fatal(err)
	}
	base = filepath.Join(dir, "base.json")
	if err := report.WriteJSON(base, r); err != nil {
		t.Fatal(err)
	}
	r.Run.ID, r.Run.Git = "5e6f7a8b-0000-4000-8000-000000000000", &report.Git{SHA: "9f3c2e1aa", Ref: "refs/pull/182/merge"}
	var tools []report.ToolStats
	for _, tl := range r.Tools {
		switch tl.Name {
		case "search":
			tl.P95 = 500
		case "big":
			continue
		case "flaky":
			tl.Reqs, tl.Errors, tl.ErrorRate = 30, 3, 0.1
		}
		tools = append(tools, tl)
	}
	r.Tools = append(tools, report.ToolStats{Name: "new", Reqs: 400, P50: 3, P95: 8, P99: 12, Max: 20})
	cur = filepath.Join(dir, "cur.json")
	if err := report.WriteJSON(cur, r); err != nil {
		t.Fatal(err)
	}
	return base, cur
}

func TestCompareGolden(t *testing.T) {
	base, cur := comparePair(t)
	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := Main(append([]string{"compare", base, cur}, args...), &out, &errb)
		if errb.Len() > 0 {
			t.Logf("stderr: %s", errb.String())
		}
		return code, strings.ReplaceAll(out.String(), filepath.Dir(base), "DIR")
	}
	code, text := run()
	wantText := `baseline: 4a1b2c3 (main) · soak · 2025-06-18 · started 2026-09-29T13:00:00Z
current:  9f3c2e1 (pull/182/merge) · soak · 2025-06-18 · started 2026-09-29T13:00:00Z
                                baseline       current          Δ     %
search (16,560 → 16,560 calls)
  p50                             118 ms        118 ms       0 ms    0%
  p95                             372 ms        500 ms    +128 ms  +34%  ⚠
  p99                             716 ms        716 ms       0 ms    0%  ·
  error rate                       0.02%         0.02%      0 pts    0%  ·
  req/s                              7.7           7.7          0    0%
fast (9,936 → 9,936 calls)
  p50                              11 ms         11 ms       0 ms    0%
  p95                              29 ms         29 ms       0 ms    0%  ·
  p99                              54 ms         54 ms       0 ms    0%  ·
  error rate                          0%            0%      0 pts    0%  ·
  req/s                              4.6           4.6          0    0%
slow (3,312 → 3,312 calls)
  p50                             303 ms        303 ms       0 ms    0%
  p95                             331 ms        331 ms       0 ms    0%  ·
  p99                             372 ms        372 ms       0 ms    0%  ·
  error rate                       0.06%         0.06%      0 pts    0%  ·
  req/s                              1.5           1.5          0    0%
flaky (3,312 → 30 calls: fewer than 50, not judged)
  p50                              39 ms         39 ms       0 ms    0%
  p95                             118 ms        118 ms       0 ms    0%
  p99                             262 ms        262 ms       0 ms    0%
  error rate                       9.81%           10%  +0.19 pts   +2%
  req/s                              1.5           0.0       -1.5  -99%
new (added: only in the current run, 400 calls)
  p50                                  –        3.0 ms
  p95                                  –        8.0 ms
  p99                                  –         12 ms
  error rate                           –            0%
  req/s                                –           0.2
big (removed: only in the baseline, 3,312 calls)
  p50                             205 ms             –
  p95                             518 ms             –
  p99                             902 ms             –
  error rate                          0%             –
  req/s                              1.5             –
run
  error rate                       0.75%         0.75%      0 pts    0%  ·
  memory growth                  1.0 MiB       1.0 MiB      0 MiB    0%  ·
  leak slope                0.03 MiB/min  0.03 MiB/min  0 MiB/min    0%  ·
  retained after cool-down      -5.2 MiB      -5.2 MiB      0 MiB        ·
rules: p95 +20% / p99 +30% and +25 ms; error rate +0.5 pts, +50% and p < 0.001; at least 50 calls per tool. ⚠ regression, ✓ improvement, · within noise
Performance regression detected vs baseline 4a1b2c3 (main):
  - ` + "`search`" + ` p95 372 ms → 500 ms (+34%, +128 ms)
`
	if code != ExitFail || text != wantText {
		t.Errorf("text: exit %d, got:\n%s\nwant:\n%s", code, text, wantText)
	}

	code, md := run("--format", "markdown")
	wantMd := "### Compared with baseline\n\n" +
		"Baseline `4a1b2c3 (main)` · soak · 2025-06-18 · started 2026-09-29T13:00:00Z\n\n" +
		"**Performance regression detected:**\n\n" +
		"- search p95 372 ms → 500 ms (+34%, +128 ms)\n\n" +
		"| Tool | Calls | p50 | p95 | p99 | Error rate | req/s |\n|---|--:|--:|--:|--:|--:|--:|\n" +
		"| `search` | 16,560 → 16,560 | 118 ms → 118 ms | 372 ms → 500 ms (+34%) ⚠ | 716 ms → 716 ms · | 0.02% → 0.02% · | 7.7 → 7.7 |\n" +
		"| `fast` | 9,936 → 9,936 | 11 ms → 11 ms | 29 ms → 29 ms · | 54 ms → 54 ms · | 0% → 0% · | 4.6 → 4.6 |\n" +
		"| `slow` | 3,312 → 3,312 | 303 ms → 303 ms | 331 ms → 331 ms · | 372 ms → 372 ms · | 0.06% → 0.06% · | 1.5 → 1.5 |\n" +
		"| `flaky` (too few calls) | 3,312 → 30 | 39 ms → 39 ms | 118 ms → 118 ms | 262 ms → 262 ms | 9.81% → 10% (+2%) | 1.5 → 0.0 |\n" +
		"| `new` (added) | – → 400 | – → 3.0 ms | – → 8.0 ms | – → 12 ms | – → 0% | – → 0.2 |\n" +
		"| `big` (removed) | 3,312 → – | 205 ms → – | 518 ms → – | 902 ms → – | 0% → – | 1.5 → – |\n\n" +
		"| Run | Baseline | Current | Δ | |\n|---|--:|--:|--:|---|\n" +
		"| error rate | 0.75% | 0.75% | 0 pts | · |\n" +
		"| memory growth | 1.0 MiB | 1.0 MiB | 0 MiB | · |\n" +
		"| leak slope | 0.03 MiB/min | 0.03 MiB/min | 0 MiB/min | · |\n" +
		"| retained after cool-down | -5.2 MiB | -5.2 MiB | 0 MiB | · |\n\n" +
		"<sub>Rules: p95 +20% / p99 +30% and +25 ms; error rate +0.5 pts, +50% and p < 0.001; at least 50 calls per tool. ⚠ regression · ✓ improvement · · within noise</sub>\n"
	if code != ExitFail || md != wantMd {
		t.Errorf("markdown: exit %d, got:\n%s\nwant:\n%s", code, md, wantMd)
	}

	// action/summary.mjs renders report.comparison for the PR comment; its test
	// (action/summary.test.mjs) renders this comparison and must match wantMd.
	_, fixture := run("--format", "json")
	// run() turns the temp dir into DIR where it appears verbatim (Linux, macOS); on
	// Windows the JSON escapes its backslashes, so replace that form too.
	fixture = strings.ReplaceAll(fixture, strings.ReplaceAll(base, `\`, `\\`), "base.json")
	fixture = strings.ReplaceAll(fixture, `"DIR/base.json"`, `"base.json"`)
	fixtures := filepath.Join("..", "..", "..", "..", "action", "testdata")
	if os.Getenv("UPDATE_FIXTURES") != "" {
		os.WriteFile(filepath.Join(fixtures, "comparison.json"), []byte(fixture), 0o644)
		os.WriteFile(filepath.Join(fixtures, "comparison.md"), []byte(wantMd), 0o644)
	}
	for name, want := range map[string]string{"comparison.json": fixture, "comparison.md": wantMd} {
		got, err := os.ReadFile(filepath.Join(fixtures, name))
		if err != nil || strings.ReplaceAll(string(got), "\r\n", "\n") != want {
			t.Errorf("action/testdata/%s is stale (rerun with UPDATE_FIXTURES=1): %v", name, err)
		}
	}

	code, js := run("--format", "json", "--max-p95-increase", "40%")
	var c report.Comparison
	if err := json.Unmarshal([]byte(js), &c); err != nil {
		t.Fatalf("json: %v\n%s", err, js)
	}
	if code != ExitPass || c.Regressed || c.Rules.MaxP95Increase != 0.4 || len(c.Tools) != 6 || (c.Baseline.Source != base && c.Baseline.Source != "DIR/"+filepath.Base(base)) {
		t.Errorf("json with --max-p95-increase 40%%: exit %d %+v", code, c)
	}
}

func TestCompareExitCodes(t *testing.T) {
	base, cur := comparePair(t)
	healthy, leaky := filepath.Join(examples, "healthy.json"), filepath.Join(examples, "leaky.json")
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"schemaVersion":"1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
		want int
		msg  string
	}{
		{"same report", []string{healthy, healthy}, ExitPass, ""},
		{"leak regresses", []string{healthy, leaky}, ExitFail, ""},
		{"min-delta-ms 200 keeps search within noise", []string{base, cur, "--min-delta-ms", "200"}, ExitPass, ""},
		{"missing file", []string{healthy, "nope.json"}, ExitError, "current:"},
		{"invalid report", []string{bad, healthy}, ExitError, "not a valid report.json"},
		{"one argument", []string{healthy}, ExitError, "Usage: mcpload compare"},
		{"bad format", []string{healthy, healthy, "--format", "html"}, ExitError, "--format"},
		{"bad percentage", []string{healthy, healthy, "--max-p95-increase", "x"}, ExitError, "percentage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if got := Main(append([]string{"compare"}, tc.args...), &out, &errb); got != tc.want || !strings.Contains(errb.String(), tc.msg) {
				t.Errorf("exit %d (want %d), stderr %q", got, tc.want, errb.String())
			}
		})
	}
}

// A bad --baseline stops `run` before k6 starts.
func TestRunBaselineReadFirst(t *testing.T) {
	var out, errb bytes.Buffer
	script := filepath.Join("..", "..", "..", "..", "scenarios", "agent-session.js")
	code := Main([]string{"run", "--url", "http://127.0.0.1:9/mcp", "--scenario", script, "--k6", "no-such-k6", "--baseline", "nope.json"}, &out, &errb)
	if code != ExitError || !strings.Contains(errb.String(), "--baseline") || strings.Contains(errb.String(), "no-such-k6") {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
	code = Main([]string{"run", "--url", "http://x/mcp", "--scenario", script, "--min-calls", "-1"}, &out, &errb)
	if code != ExitError {
		t.Errorf("negative --min-calls: exit %d", code)
	}
}

func TestPctFlag(t *testing.T) {
	var v float64
	f := pctFlag{&v}
	for in, want := range map[string]float64{"20%": 0.2, "20": 0.2, " 5.5% ": 0.055, "0": 0} {
		if err := f.Set(in); err != nil || v != want {
			t.Errorf("Set(%q) = %v, %v", in, v, err)
		}
	}
	for _, in := range []string{"-1%", "abc", "NaN"} {
		if err := f.Set(in); err == nil {
			t.Errorf("Set(%q) accepted", in)
		}
	}
	if def := analysis.DefaultCompareConfig(); (pctFlag{&def.MaxP95Increase}).String() != "20%" {
		t.Errorf("String() = %q", pctFlag{&def.MaxP95Increase}.String())
	}
}
