package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/k6run"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/workload"
)

func TestRunWorkloadFlag(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	os.WriteFile(bad, []byte("workload:\n  name: w\n  flows:\n    - name: f\n      weight: 1\n      steps: [{ name: s, tool: a, agrs: {} }]\n"), 0o600)
	o := &runOpts{url: "http://u/mcp", workload: bad, set: map[string]bool{}}
	err := o.validate(nil)
	if err == nil || !strings.Contains(err.Error(), "flow `f` step 1: unknown key 'agrs'") {
		t.Errorf("bad workload: %v", err)
	}

	good := filepath.Join(dir, "good.yaml")
	os.WriteFile(good, []byte("name: w\nflows: [{name: f, weight: 1, steps: [fast]}]\n"), 0o600)
	// --workload picks the workload scenario when --scenario is not given.
	o = &runOpts{url: "http://u/mcp", workload: good, set: map[string]bool{}}
	if err := o.validate(nil); err == nil || !strings.Contains(err.Error(), `scenario "workload"`) {
		t.Errorf("default scenario: %v", err)
	}
	if o.wl == nil || o.wl.Name != "w" {
		t.Errorf("workload not loaded: %+v", o.wl)
	}
	script := filepath.Join(dir, "custom.js")
	os.WriteFile(script, []byte("export default function () {}\n"), 0o600)
	o = &runOpts{url: "http://u/mcp", workload: good, scenario: script, samplerKind: "none", interval: 10 * time.Second, leakSlope: 1, minR2: 0.7, set: map[string]bool{}}
	if err := o.validate(nil); err != nil || o.scenario != script {
		t.Errorf("explicit scenario: %v %s", err, o.scenario)
	}
}

func TestWorkloadFileEnv(t *testing.T) {
	w, err := workload.Parse([]byte("name: w\nflows: [{name: f, weight: 1, steps: [fast]}]\n"), ".")
	if err != nil {
		t.Fatal(err)
	}
	path, cleanup, err := writeWorkloadFile(w)
	if err != nil {
		t.Fatal(err)
	}
	if back, err := workload.ReadNormalized(path); err != nil || back.Name != "w" || !filepath.IsAbs(path) {
		t.Errorf("written file: %v %+v", err, back)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("cleanup left the file")
	}

	o := &runOpts{url: "http://u/mcp", protocol: "auto", wlFile: "/tmp/wl.json", env: multiFlag{"WORKLOAD_FILE=other.json"}, set: map[string]bool{}}
	if _, m := o.k6Env(); m[workloadFileEnv] != "/tmp/wl.json" {
		t.Errorf("--workload must win: %v", m[workloadFileEnv])
	}
	// k6's open() resolves relative paths against the script; mcpload passes an absolute one.
	o = &runOpts{url: "http://u/mcp", protocol: "auto", env: multiFlag{"WORKLOAD_FILE=wl.json"}, set: map[string]bool{}}
	cwd, _ := os.Getwd()
	if _, m := o.k6Env(); m[workloadFileEnv] != filepath.Join(cwd, "wl.json") {
		t.Errorf("relative WORKLOAD_FILE = %v", m[workloadFileEnv])
	}
}

func TestWorkloadReportAndTable(t *testing.T) {
	def, err := workload.Parse([]byte(`
name: customer-support
budgets: { p95: 2s, completion: 99% }
flows:
  - { name: lookup-orders, weight: 60, steps: [search_customer, { name: get_orders, tool: search }] }
  - { name: create-ticket, weight: 30, budgets: { steps: { create_ticket: { p95: 1s } } }, steps: [{ name: create_ticket, tool: slow }] }
  - { name: rare, weight: 10, steps: [fast] }
`), ".")
	if err != nil {
		t.Fatal(err)
	}
	lat := func(n int64, p50, p95, p99 float64) k6run.Latency {
		return k6run.Latency{Count: n, P50: p50, P95: p95, P99: p99, Max: p99}
	}
	stats := []k6run.FlowStats{
		{Name: "create-ticket", WorkflowStats: k6run.WorkflowStats{Runs: 40, Completed: 37, Duration: lat(37, 1000, 4100, 4400),
			Steps: []k6run.WorkflowStep{{Name: "create_ticket", Latency: lat(40, 900, 1200, 1300)}}}},
		{Name: "lookup-orders", WorkflowStats: k6run.WorkflowStats{Runs: 120, Completed: 120, Duration: lat(120, 500, 1400, 1800),
			Steps: []k6run.WorkflowStep{{Name: "search_customer", Latency: lat(120, 4, 9, 11)}}}},
	}
	r := workloadReport(def, stats)
	if len(r.Flows) != 3 || r.Flows[0].Name != "lookup-orders" || r.Flows[2].Runs != 0 {
		t.Fatalf("flows = %+v", r.Flows)
	}
	lo := r.Flows[0]
	// plan order; get_orders never ran (count 0)
	if len(lo.Steps) != 2 || lo.Steps[1].Name != "get_orders" || lo.Steps[1].Count != 0 || *lo.Budget.P95Ms != 2000 || *lo.Budget.MinCompletionRate != 0.99 {
		t.Errorf("lookup-orders = %+v", lo)
	}
	ct := r.Flows[1]
	if ct.CompletionRate != 0.925 || ct.Steps[0].Budget == nil || *ct.Steps[0].Budget.P95Ms != 1000 {
		t.Errorf("create-ticket = %+v", ct)
	}

	var buf bytes.Buffer
	printWorkload(&buf, r)
	out := buf.String()
	for _, want := range []string{
		"mcpload workload customer-support (3 flows, 160 runs; end-to-end times of completed flows):",
		"  flow           weight    runs  completed       p50       p95       p99  slowest step (p95)",
		"  lookup-orders     60%     120     100.0%    500 ms     1.4 s     1.8 s  search_customer 9.0 ms",
		"  create-ticket     30%      40      92.5%       1 s     4.1 s     4.4 s  create_ticket 1.2 s",
		"  rare              10%       0          -         -         -         -  -",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
}
