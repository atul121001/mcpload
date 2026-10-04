package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/analysis"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

const compareSynopsis = "mcpload compare <baseline.json> <current.json> [--max-p95-increase 20%] [--min-delta-ms 25] [--min-calls 50] [--format text|markdown|json]"

// pctFlag is a percentage flag: "20%" and "20" both mean 0.2.
type pctFlag struct{ v *float64 }

func (p pctFlag) String() string {
	if p.v == nil {
		return ""
	}
	return strconv.FormatFloat(*p.v*100, 'f', -1, 64) + "%"
}

func (p pctFlag) Set(s string) error {
	f, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64)
	if err != nil || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("want a percentage >= 0 such as 20%% (got %q)", s)
	}
	*p.v = f / 100
	return nil
}

// compareFlags registers the comparison rules on fs (compare and run --baseline).
func compareFlags(fs *flag.FlagSet, cc *analysis.CompareConfig) {
	fs.Var(pctFlag{&cc.MaxP95Increase}, "max-p95-increase", "a tool's p95 regresses when it rose by more than this (and by more than --min-delta-ms)")
	fs.Var(pctFlag{&cc.MaxP99Increase}, "max-p99-increase", "the same for p99")
	fs.Var(pctFlag{&cc.MaxErrorIncrease}, "max-error-increase", "an error rate regresses when it rose by more than this share of the baseline rate, by more than --min-error-delta points and significantly (two-proportion z-test, p < 0.001)")
	fs.Var(pctFlag{&cc.MinErrorDelta}, "min-error-delta", "absolute error-rate floor in percentage points")
	fs.Float64Var(&cc.MinDeltaMs, "min-delta-ms", cc.MinDeltaMs, "absolute latency floor: smaller changes are noise")
	fs.Int64Var(&cc.MinCalls, "min-calls", cc.MinCalls, "a tool is judged only when both runs made at least this many calls of it")
	fs.Float64Var(&cc.MaxLeakSlopeIncrease, "max-leak-slope-increase", cc.MaxLeakSlopeIncrease, "memory leak slope regresses when it rose by more than this many MiB/min")
}

func checkCompareConfig(cc analysis.CompareConfig) error {
	if cc.MinDeltaMs < 0 || cc.MinCalls < 0 || cc.MaxLeakSlopeIncrease < 0 {
		return errors.New("--min-delta-ms, --min-calls and --max-leak-slope-increase must not be negative")
	}
	return nil
}

// loadReport reads a report.json from a path or an http(s) URL and checks it.
func loadReport(src string) (*report.Report, error) {
	var r *report.Report
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "mcpload/"+Version)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: HTTP %d", src, resp.StatusCode)
		}
		r = &report.Report{}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<20)).Decode(r); err != nil {
			return nil, fmt.Errorf("parse %s: %w", src, err)
		}
	} else {
		var err error
		if r, err = report.ReadJSON(src); err != nil {
			return nil, err
		}
	}
	if err := r.Check(); err != nil {
		return nil, fmt.Errorf("%s is not a valid report.json: %w", src, err)
	}
	return r, nil
}

func compareCmd(args []string, stdout, stderr io.Writer) int {
	cc := analysis.DefaultCompareConfig()
	fs := newFlagSet("compare", compareSynopsis, stderr)
	compareFlags(fs, &cc)
	format := fs.String("format", "text", "output format: text, markdown or json")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) != 2 {
		fs.Usage()
		return ExitError
	}
	if err := checkCompareConfig(cc); err != nil {
		fmt.Fprintf(stderr, "mcpload compare: %v\n", err)
		return ExitError
	}
	if *format != "text" && *format != "markdown" && *format != "json" {
		fmt.Fprintf(stderr, "mcpload compare: --format must be text, markdown or json (got %q)\n", *format)
		return ExitError
	}
	base, err := loadReport(pos[0])
	if err != nil {
		fmt.Fprintf(stderr, "mcpload compare: baseline: %v\n", err)
		return ExitError
	}
	cur, err := loadReport(pos[1])
	if err != nil {
		fmt.Fprintf(stderr, "mcpload compare: current: %v\n", err)
		return ExitError
	}
	c := analysis.Compare(base, cur, redactURL(pos[0]), cc, analysis.DefaultConfig())
	switch *format {
	case "json":
		cr := &report.Report{Comparison: c}
		cr.Normalize()
		b, err := json.MarshalIndent(cr.Comparison, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "mcpload compare: %v\n", err)
			return ExitError
		}
		fmt.Fprintf(stdout, "%s\n", b)
	case "markdown":
		io.WriteString(stdout, CompareMarkdown(c))
	default:
		writeCompareText(stdout, c, cur)
	}
	if c.Regressed {
		return ExitFail
	}
	return ExitPass
}

// marker is the symbol of a delta status: ⚠ regression, ✓ improvement, · within noise.
func marker(st string) string {
	switch st {
	case report.DeltaRegressed:
		return "⚠"
	case report.DeltaImproved:
		return "✓"
	case report.DeltaOK:
		return "·"
	}
	return ""
}

// fieldStatus is the status of one judged tool field ("p95", "p99", "errorRate").
func fieldStatus(t report.ToolDelta, field string, judged bool) string {
	if !judged {
		return ""
	}
	for _, f := range t.Regressed {
		if f == field {
			return report.DeltaRegressed
		}
	}
	for _, f := range t.Improved {
		if f == field {
			return report.DeltaImproved
		}
	}
	return report.DeltaOK
}

// latencyJudged reports whether the tool's latency was judged (enough successful calls).
func latencyJudged(t report.ToolDelta, minCalls int64) bool {
	return t.Base != nil && t.Current != nil && t.Status != report.DeltaFewCalls &&
		t.Base.Reqs-t.Base.Errors >= minCalls && t.Current.Reqs-t.Current.Errors >= minCalls
}

func errorJudged(t report.ToolDelta) bool {
	return t.Base != nil && t.Current != nil && t.Status != report.DeltaFewCalls
}

// pctOf formats the relative change "+35%" (blank without both values; see analysis.PctChange).
func pctOf(base, cur *float64) string {
	if base == nil || cur == nil {
		return ""
	}
	return analysis.PctChange(*base, *cur)
}

func deltaOf(unit string, base, cur *float64) string {
	if base == nil || cur == nil {
		return ""
	}
	if unit == "req/s" {
		if d := *cur - *base; math.Abs(d) >= 0.05 {
			return fmt.Sprintf("%+.1f", d)
		}
		return "0"
	}
	return analysis.FormatDelta(unit, *cur-*base)
}

func valueOf(unit string, v *float64) string {
	if unit == "req/s" {
		if v == nil {
			return "–"
		}
		return fmt.Sprintf("%.1f", *v)
	}
	return analysis.FormatValue(unit, v)
}

// cmpRow is one line of the comparison table.
type cmpRow struct {
	label, unit string
	base, cur   *float64
	status      string // "" = not judged
}

func (r cmpRow) cells() []string {
	return []string{r.label, valueOf(r.unit, r.base), valueOf(r.unit, r.cur), deltaOf(r.unit, r.base, r.cur), pctOf(r.base, r.cur), marker(r.status)}
}

func toolRows(t report.ToolDelta, minCalls int64) []cmpRow {
	get := func(s *report.ToolSample, f func(*report.ToolSample) float64) *float64 {
		if s == nil {
			return nil
		}
		return report.F(f(s))
	}
	lat, errs := latencyJudged(t, minCalls), errorJudged(t)
	return []cmpRow{
		{"p50", "ms", get(t.Base, func(s *report.ToolSample) float64 { return s.P50 }), get(t.Current, func(s *report.ToolSample) float64 { return s.P50 }), ""},
		{"p95", "ms", get(t.Base, func(s *report.ToolSample) float64 { return s.P95 }), get(t.Current, func(s *report.ToolSample) float64 { return s.P95 }), fieldStatus(t, "p95", lat)},
		{"p99", "ms", get(t.Base, func(s *report.ToolSample) float64 { return s.P99 }), get(t.Current, func(s *report.ToolSample) float64 { return s.P99 }), fieldStatus(t, "p99", lat)},
		{"error rate", "rate", get(t.Base, func(s *report.ToolSample) float64 { return s.ErrorRate }), get(t.Current, func(s *report.ToolSample) float64 { return s.ErrorRate }), fieldStatus(t, "errorRate", errs)},
		{"req/s", "req/s", get(t.Base, func(s *report.ToolSample) float64 { return s.RPS }), get(t.Current, func(s *report.ToolSample) float64 { return s.RPS }), ""},
	}
}

// toolHeading is "search (1,204 → 1,180 calls)", or says why the tool was not judged.
func toolHeading(t report.ToolDelta, minCalls int64) string {
	switch t.Status {
	case report.DeltaAdded:
		return fmt.Sprintf("%s (added: only in the current run, %s calls)", t.Name, fmtCount(t.Current.Reqs))
	case report.DeltaRemoved:
		return fmt.Sprintf("%s (removed: only in the baseline, %s calls)", t.Name, fmtCount(t.Base.Reqs))
	case report.DeltaFewCalls:
		return fmt.Sprintf("%s (%s → %s calls: fewer than %d, not judged)", t.Name, fmtCount(t.Base.Reqs), fmtCount(t.Current.Reqs), minCalls)
	}
	return fmt.Sprintf("%s (%s → %s calls)", t.Name, fmtCount(t.Base.Reqs), fmtCount(t.Current.Reqs))
}

// fmtCount formats n with thousands separators.
func fmtCount(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func describeRun(r *report.Report) string {
	return fmt.Sprintf("%s, %s, %.0f s", r.Run.Scenario, r.Run.Protocol, r.Run.DurationS)
}

// rulesText summarises the noise rules in one line.
func rulesText(ru report.CompareRules) string {
	return fmt.Sprintf("p95 +%g%% / p99 +%g%% and +%g ms; error rate +%g pts, +%g%% and p < 0.001; at least %d calls per tool",
		round6(ru.MaxP95Increase*100), round6(ru.MaxP99Increase*100), ru.MinDeltaMs, round6(ru.MinErrorDelta*100), round6(ru.MaxErrorIncrease*100), ru.MinCalls)
}

// writeCompareText prints the comparison as a table: one block per tool,
// then the run-level metrics, then the result.
func writeCompareText(w io.Writer, c *report.Comparison, cur *report.Report) {
	b := c.Baseline
	fmt.Fprintf(w, "baseline: %s · %s · %s · started %s\n", analysis.BaselineName(b), b.Scenario, b.Protocol, b.StartedAt)
	fmt.Fprintf(w, "current:  %s · %s · %s · started %s\n", currentName(cur), cur.Run.Scenario, cur.Run.Protocol, cur.Run.StartedAt)
	for _, warn := range c.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warn)
	}
	var lines [][]string
	head := []string{"", "baseline", "current", "Δ", "%", ""}
	lines = append(lines, head)
	group := func(title string, rows []cmpRow) {
		lines = append(lines, []string{title})
		for _, r := range rows {
			cells := r.cells()
			cells[0] = "  " + cells[0]
			lines = append(lines, cells)
		}
	}
	for _, t := range c.Tools {
		group(toolHeading(t, c.Rules.MinCalls), toolRows(t, c.Rules.MinCalls))
	}
	if len(c.Metrics) > 0 {
		lines = append(lines, []string{"run"})
		for _, m := range c.Metrics {
			st := m.Status
			if st == report.DeltaNA {
				st = ""
			}
			cells := cmpRow{m.Label, m.Unit, m.Base, m.Current, st}.cells()
			cells[0] = "  " + cells[0]
			lines = append(lines, cells)
			if m.Note != "" {
				lines = append(lines, []string{"    (" + m.Note + ")"})
			}
		}
	}
	writeTable(w, lines)
	fmt.Fprintf(w, "rules: %s. ⚠ regression, ✓ improvement, · within noise\n", rulesText(c.Rules))
	if c.Regressed {
		fmt.Fprintf(w, "Performance regression detected vs baseline %s:\n", analysis.BaselineName(b))
		for _, r := range c.Reasons {
			fmt.Fprintf(w, "  - %s\n", r)
		}
		return
	}
	fmt.Fprintf(w, "No regression vs baseline %s (every difference is within noise or an improvement).\n", analysis.BaselineName(b))
}

func currentName(r *report.Report) string {
	return analysis.BaselineName(report.BaselineRef{RunID: r.Run.ID, Git: r.Run.Git})
}

// writeTable prints rows with the first column left-aligned and the others
// right-aligned; a row with one cell is a group heading.
func writeTable(w io.Writer, rows [][]string) {
	width := make([]int, 6)
	for _, r := range rows {
		if len(r) == 1 {
			continue
		}
		for i, c := range r {
			width[i] = max(width[i], len([]rune(c)))
		}
	}
	for _, r := range rows {
		if len(r) == 1 {
			fmt.Fprintln(w, r[0])
			continue
		}
		var sb strings.Builder
		for i, c := range r {
			pad := strings.Repeat(" ", width[i]-len([]rune(c)))
			switch {
			case i == 0:
				sb.WriteString(c + pad)
			case i == len(r)-1:
				sb.WriteString("  " + c)
			default:
				sb.WriteString("  " + pad + c)
			}
		}
		fmt.Fprintln(w, strings.TrimRight(sb.String(), " "))
	}
}

// mdCell is "210 ms → 284 ms (+35%) ⚠" for one tool field.
func mdCell(r cmpRow) string {
	switch {
	case r.base == nil:
		return "– → " + valueOf(r.unit, r.cur)
	case r.cur == nil:
		return valueOf(r.unit, r.base) + " → –"
	}
	s := valueOf(r.unit, r.base) + " → " + valueOf(r.unit, r.cur)
	if p := pctOf(r.base, r.cur); p != "" && p != "0%" && r.unit != "req/s" {
		s += " (" + p + ")"
	}
	if m := marker(r.status); m != "" {
		s += " " + m
	}
	return s
}

// mdSanitize scrubs a reason/warning string before it goes into Markdown that
// may be posted to a PR comment. It drops the characters that could break out
// of a table cell or a code span (backtick, pipe, HTML brackets, @mention) and
// escapes the remaining link/image markers. Bold/italic markers (* and _) are
// left alone: they are harmless and escaping them turns identifiers such as
// `memory_leak` into `memory\_leak`, which breaks grep in CI and looks ugly in
// the comment. Kept in sync with action/summary.mjs cell(). CodeQL #3; L4.
func mdSanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
			b.WriteByte(' ')
		case r == '`' || r == '<' || r == '>' || r == '|' || r == '@':
			// drop
		case r == '\\' || r == '[' || r == ']' || r == '!':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// CompareMarkdown renders the comparison for a PR comment or job summary:
// a per-tool table (one row per tool) and a run-level table. action/summary.mjs
// renders report.comparison the same way.
func CompareMarkdown(c *report.Comparison) string {
	var sb strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&sb, format, a...) }
	b := c.Baseline
	p("### Compared with baseline\n\n")
	p("Baseline `%s` · %s · %s · started %s\n\n", analysis.BaselineName(b), b.Scenario, b.Protocol, b.StartedAt)
	if c.Regressed {
		p("**Performance regression detected:**\n\n")
		for _, r := range c.Reasons {
			p("- %s\n", mdSanitize(r))
		}
		p("\n")
	} else {
		p("**No regression:** every difference is within noise or an improvement.\n\n")
	}
	for _, warn := range c.Warnings {
		p("> ⚠ %s\n", mdSanitize(warn))
	}
	if len(c.Warnings) > 0 {
		p("\n")
	}
	if len(c.Tools) > 0 {
		p("| Tool | Calls | p50 | p95 | p99 | Error rate | req/s |\n|---|--:|--:|--:|--:|--:|--:|\n")
		for _, t := range c.Tools {
			name := "`" + strings.ReplaceAll(t.Name, "|", `\|`) + "`"
			switch t.Status {
			case report.DeltaAdded:
				name += " (added)"
			case report.DeltaRemoved:
				name += " (removed)"
			case report.DeltaFewCalls:
				name += " (too few calls)"
			}
			var calls string
			switch {
			case t.Base == nil:
				calls = "– → " + fmtCount(t.Current.Reqs)
			case t.Current == nil:
				calls = fmtCount(t.Base.Reqs) + " → –"
			default:
				calls = fmtCount(t.Base.Reqs) + " → " + fmtCount(t.Current.Reqs)
			}
			cells := []string{name, calls}
			for _, r := range toolRows(t, c.Rules.MinCalls) {
				cells = append(cells, mdCell(r))
			}
			p("| %s |\n", strings.Join(cells, " | "))
		}
		p("\n")
	}
	if len(c.Metrics) > 0 {
		p("| Run | Baseline | Current | Δ | |\n|---|--:|--:|--:|---|\n")
		for _, m := range c.Metrics {
			d := deltaOf(m.Unit, m.Base, m.Current)
			if pc := pctOf(m.Base, m.Current); d != "" && pc != "" && pc != "0%" {
				d += " (" + pc + ")"
			}
			st := m.Status
			if st == report.DeltaNA {
				st = ""
			}
			mark := marker(st)
			if m.Note != "" {
				mark = strings.TrimSpace(mark + " " + m.Note)
			}
			p("| %s | %s | %s | %s | %s |\n", m.Label, valueOf(m.Unit, m.Base), valueOf(m.Unit, m.Current), d, mark)
		}
		p("\n")
	}
	p("<sub>Rules: %s. ⚠ regression · ✓ improvement · · within noise</sub>\n", rulesText(c.Rules))
	return sb.String()
}
