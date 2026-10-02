package k6run

import (
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func val(t *testing.T, p *float64) float64 {
	t.Helper()
	if p == nil {
		t.Fatal("unexpected nil")
	}
	return *p
}

func TestPercentileMatchesK6(t *testing.T) {
	s := []float64{5, 10, 20, 30}
	if got := Percentile(s, 0.95); !near(got, 28.5) {
		t.Errorf("p95 = %v, want 28.5", got)
	}
	if got := Percentile(s, 0.5); !near(got, 15) {
		t.Errorf("p50 = %v, want 15", got)
	}
	if Percentile(nil, 0.5) != 0 || Percentile([]float64{7}, 0.99) != 7 {
		t.Error("edge cases")
	}
}

func parseFixture(t *testing.T, track []string) *Aggregator {
	t.Helper()
	a, err := ParseFile("testdata/run.ndjson", 10*time.Second, track)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestParseFileOriginAndSeries(t *testing.T) {
	a := parseFixture(t, nil)
	want := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	if !a.First().Equal(want) || !a.Origin.Equal(want) {
		t.Fatalf("origin = %v, want %v", a.Origin, want)
	}
	if !a.Last().Equal(want.Add(25 * time.Second)) {
		t.Fatalf("last = %v", a.Last())
	}
	if a.Buckets() != 2 {
		t.Fatalf("buckets = %d, want 2", a.Buckets())
	}
	c := a.Client(3)
	if got := val(t, c.P95Ms[0]); !near(got, 28.5) {
		t.Errorf("bucket0 p95 = %v", got)
	}
	if got := val(t, c.P95Ms[1]); !near(got, 274) {
		t.Errorf("bucket1 p95 = %v", got)
	}
	if c.P95Ms[2] != nil || c.ErrorRate[2] != nil {
		t.Error("empty bucket must be null")
	}
	if got := val(t, c.ErrorRate[0]); !near(got, 0.25) {
		t.Errorf("bucket0 err = %v", got)
	}
	if got := val(t, c.ErrorRate[1]); !near(got, 1.0/3) {
		t.Errorf("bucket1 err = %v", got)
	}
	if got := val(t, c.RPS[0]); !near(got, 0.4) {
		t.Errorf("bucket0 rps = %v", got)
	}
	if got := val(t, c.RPS[2]); got != 0 {
		t.Errorf("bucket2 rps = %v", got)
	}
	// n larger than data pads, n smaller folds the tail into the last bucket.
	if c5 := a.Client(5); len(c5.RPS) != 5 || *c5.RPS[4] != 0 {
		t.Error("padding")
	}
	if c1 := a.Client(1); !near(*c1.RPS[0], 0.7) || !near(*c1.ErrorRate[0], 2.0/7) {
		t.Errorf("fold: rps %v err %v", *c1.RPS[0], *c1.ErrorRate[0])
	}
}

func TestSummaryToolsProtocol(t *testing.T) {
	a := parseFixture(t, nil)
	s := a.Summary()
	if s.Reqs != 7 || s.Errors != 2 || !near(s.ErrorRate, 2.0/7) {
		t.Errorf("summary = %+v", s)
	}
	if s.ByErrorType["tool_iserror"] != 1 || s.ByErrorType["session_not_found"] != 1 || len(s.ByErrorType) != 2 {
		t.Errorf("byErrorType = %v", s.ByErrorType)
	}
	tools := a.Tools()
	if len(tools) != 3 || tools[0].Name != "flaky" || tools[1].Name != "search" || tools[2].Name != "slow" {
		t.Fatalf("tools = %+v", tools)
	}
	fl, se := tools[0], tools[1]
	if fl.Reqs != 1 || fl.Errors != 1 || fl.ErrorRate != 1 || fl.P50 != 5 {
		t.Errorf("flaky = %+v", fl)
	}
	if se.Reqs != 4 || se.Errors != 1 || !near(se.ErrorRate, 0.25) || !near(se.P50, 25) || !near(se.P95, 38.5) || se.Max != 40 {
		t.Errorf("search = %+v", se)
	}
	if !(se.P50 <= se.P95 && se.P95 <= se.P99 && se.P99 <= se.Max) {
		t.Error("percentiles not monotonic")
	}
	// the auto-probe used 2026-07-28 but the successful connect negotiated 2025-11-25
	if p := a.Protocol(); p != "2025-11-25" {
		t.Errorf("protocol = %q", p)
	}
}

func TestThresholds(t *testing.T) {
	sum, err := ReadSummary("testdata/summary.json")
	if err != nil {
		t.Fatal(err)
	}
	defs := []ThresholdDef{
		{"mcp_connect_duration", "p(95)<1500"},
		{"mcp_req_duration{tool:search}", "p(95)<30"},
		{"mcp_tool_error_rate{tool:flaky}", "rate<0.15"},
		{"mcp_req_duration{tool:nosuch}", "p(99)<10"},
	}
	keys := []string{}
	for _, d := range defs {
		keys = append(keys, d.Metric)
	}
	a := parseFixture(t, keys)
	ths := a.Thresholds(defs, sum, 25)
	if len(ths) != 4 {
		t.Fatalf("thresholds = %+v", ths)
	}
	if !ths[0].Passed || !near(val(t, ths[0].Observed), 50) {
		t.Errorf("connect = %+v", ths[0])
	}
	// k6 said failed (true in summary export)
	if ths[1].Passed || !near(val(t, ths[1].Observed), 38.5) {
		t.Errorf("search = %+v", ths[1])
	}
	// not in summary: evaluated locally from observed rate 1.0
	if ths[2].Passed || val(t, ths[2].Observed) != 1 {
		t.Errorf("flaky = %+v", ths[2])
	}
	if !ths[3].Passed || ths[3].Observed != nil {
		t.Errorf("no samples = %+v", ths[3])
	}
}

func TestThresholdsFromSummaryOnly(t *testing.T) {
	sum, _ := ParseSummary([]byte(`{"metrics":{"mcp_errors{error_type:session_not_found}":{"count":1,"thresholds":{"count<1":true}}}}`))
	a := parseFixture(t, []string{"mcp_errors{error_type:session_not_found}"})
	ths := a.Thresholds(nil, sum, 25)
	if len(ths) != 1 || ths[0].Passed || val(t, ths[0].Observed) != 1 {
		t.Errorf("got %+v", ths)
	}
}

func TestParseExprAndSelector(t *testing.T) {
	e, err := ParseExpr("p(99.9) <= 2000")
	if err != nil || e.Agg != "p(99.9)" || e.Op != "<=" || e.Value != 2000 {
		t.Errorf("expr = %+v %v", e, err)
	}
	if _, err := ParseExpr("nonsense"); err == nil {
		t.Error("want error")
	}
	s, err := ParseSelector("mcp_req_duration{tool:search, method:tools/call}")
	if err != nil || s.Metric != "mcp_req_duration" || s.Tags["tool"] != "search" || s.Tags["method"] != "tools/call" {
		t.Errorf("selector = %+v %v", s, err)
	}
	if !s.Matches("mcp_req_duration", map[string]string{"tool": "search", "method": "tools/call", "x": "y"}) ||
		s.Matches("mcp_req_duration", map[string]string{"tool": "search"}) {
		t.Error("matches")
	}
}

func TestOptionsSoak(t *testing.T) {
	b, err := os.ReadFile("testdata/inspect-soak.json")
	if err != nil {
		t.Fatal(err)
	}
	o, err := ParseOptions(b)
	if err != nil {
		t.Fatal(err)
	}
	if o.ScenarioName() != "soak" {
		t.Errorf("scenario name %q", o.ScenarioName())
	}
	if len(o.Thresholds) != 16 || o.Thresholds[0].Metric != "mcp_connect_duration" {
		t.Errorf("thresholds = %d %+v", len(o.Thresholds), o.Thresholds[:1])
	}
	name, _ := o.MainScenario()
	l := o.LoadShape()
	if name != "load" || l.Executor != "constant-arrival-rate" || *l.VUs != 20 || *l.MaxVUs != 200 || *l.ArrivalRate != 2 || *l.ArrivalTimeUnitS != 1 {
		t.Errorf("load = %s %+v", name, l)
	}
}

func TestOptionsAgentAndObjectThresholds(t *testing.T) {
	b, err := os.ReadFile("testdata/inspect-agent.json")
	if err != nil {
		t.Fatal(err)
	}
	o, err := ParseOptions(b)
	if err != nil {
		t.Fatal(err)
	}
	l := o.LoadShape()
	if l.Executor != "constant-vus" || l.VUs == nil || *l.VUs != 10 {
		t.Errorf("load = %+v", l)
	}
	o, err = ParseOptions([]byte(`{"thresholds":{"m":["a<1",{"threshold":"b<2","abortOnFail":true}]}}`))
	if err != nil || len(o.Thresholds) != 2 || o.Thresholds[1].Expr != "b<2" {
		t.Errorf("got %+v %v", o, err)
	}
	var nilOpts *Options
	if l := nilOpts.LoadShape(); l.Executor == "" {
		t.Error("fallback load")
	}
}

func TestAlignServer(t *testing.T) {
	o := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	f := func(v float64) *float64 { return &v }
	pts := []ServerPoint{
		{T: o.Add(-3 * time.Second), RSSBytes: f(100)}, // just before start -> bucket 0
		{T: o.Add(-time.Minute), RSSBytes: f(1)},       // far before -> dropped
		{T: o.Add(4 * time.Second), RSSBytes: f(200), ActiveSessions: f(3)},
		{T: o.Add(25 * time.Second), RSSBytes: f(400)},
		{T: o.Add(31 * time.Second), RSSBytes: f(500)}, // past the end -> last bucket
	}
	s := AlignServer(pts, o, 10*time.Second, 3)
	if len(s.RSSBytes) != 3 || *s.RSSBytes[0] != 150 || s.RSSBytes[1] != nil || *s.RSSBytes[2] != 450 {
		t.Errorf("rss = %v", s.RSSBytes)
	}
	if s.HeapBytes != nil || s.OpenFDs != nil || len(s.ActiveSessions) != 3 || *s.ActiveSessions[0] != 3 {
		t.Errorf("optional series: %+v", s)
	}
	if ts := Times(3, 10*time.Second); ts[2] != 20 {
		t.Errorf("times = %v", ts)
	}
}

func TestArgsAndVersion(t *testing.T) {
	args := RunConfig{Script: "s.js", NDJSONPath: "o.ndjson", SummaryPath: "s.json", Env: []string{"A=1"}}.Args()
	got := strings.Join(args, " ")
	if !strings.HasPrefix(got, "run --out json=o.ndjson --summary-export s.json") || !strings.HasSuffix(got, "-e A=1 s.js") {
		t.Errorf("args = %s", got)
	}
	if v := parseVersion("k6.exe v2.3.0 (go1.27.0, windows/amd64)\nExtensions:\n"); v != "v2.3.0" {
		t.Errorf("version = %q", v)
	}
}

func TestSanitizeKey(t *testing.T) {
	if sanitizeKey("Session-Not Found") != "session_not_found" || sanitizeKey("9x") != "e_9x" {
		t.Error(sanitizeKey("Session-Not Found"), sanitizeKey("9x"))
	}
}
