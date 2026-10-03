package workload

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadExample(t *testing.T) {
	w, err := Load(filepath.Join("..", "..", "..", "..", "examples", "workloads", "customer-support.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if w.Name != "customer-support" || w.Agents != 20 || w.Duration != "1m" || w.ThinkMs == nil || *w.ThinkMs != 300 {
		t.Errorf("header = %+v", w)
	}
	var names []string
	for _, f := range w.Flows {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "lookup-orders,check-subscription,create-ticket" {
		t.Errorf("flows = %v", names)
	}
	if got := strings.Join(w.ToolNames(), ","); got != "search,big,fast,slow,flaky" {
		t.Errorf("tools = %s", got)
	}
	c := w.Data["customers"]
	if c == nil || c.Pick != PickRandom || len(c.Rows) != 12 || c.Rows[0].(map[string]any)["email"] != "ada@example.com" {
		t.Fatalf("customers = %+v", c)
	}
	// Budgets: workload defaults, flow overrides, step budgets only where set.
	lo, ct := w.Flows[0], w.Flows[2]
	if lo.Budgets.P95 != 2000 || lo.Budgets.Completion != 0.99 || lo.Budgets.P99 != nil || lo.Budgets.Steps != nil {
		t.Errorf("lookup-orders budgets = %+v", lo.Budgets)
	}
	if ct.Budgets.P95 != 3000 || ct.Budgets.Completion != 0.95 || *ct.Budgets.Steps["create_ticket"].P95 != 1000 {
		t.Errorf("create-ticket budgets = %+v", ct.Budgets)
	}
	// The parallel step keeps both calls; forEach gets its default-free max as written.
	det := lo.Steps[2]
	if det.Name != "get_order_details" || len(det.Calls) != 2 || det.Calls[0].ForEach["max"] != 3 || det.Calls[1].Tool != "big" {
		t.Errorf("get_order_details = %+v", det)
	}
}

func TestLoadTemplate(t *testing.T) {
	w, err := Load(filepath.Join("..", "..", "..", "..", "examples", "workloads", "template.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Flows) != 3 || w.Data["regions"].Rows[1] != "us-east" || w.Flows[2].Budgets.Completion != 0.995 {
		t.Errorf("template = %+v", w)
	}
	if c := w.Flows[2].Steps[2].Calls[0]; c.Tool != "add_ticket_note" || c.Repeat != 2 {
		t.Errorf("repeat step = %+v", c)
	}
}

func TestParseNormalizes(t *testing.T) {
	src := `
name: shop
data:
  regions: { values: [eu, us], pick: sequential }
flows:
  - name: browse
    weight: 2.5
    thinkMs: 0
    budgets: { p95: 1500, p99: "3s", completion: 0.9, stepP95: 400ms, steps: { search-2: { p95: 900 } } }
    steps:
      - search
      - search
      - tool: get item
        args: { region: "{{data.regions}}", n: 3, nested: { list: [1, "{{data.regions}}"] } }
        as: item
        thinkMs: 50
      - parallel:
          - fast
          - { tool: search, forEach: { $from: item, path: results }, args: { q: { $from: $item, path: title } } }
      - name: search
        tool: other
`
	w, err := Parse([]byte(src), ".")
	if err != nil {
		t.Fatal(err)
	}
	f := w.Flows[0]
	var names []string
	for _, s := range f.Steps {
		names = append(names, s.Name)
	}
	// Explicit names are reserved first; defaults come from the tool (sanitized) or step<N> for a parallel step.
	if got := strings.Join(names, ","); got != "search-2,search-3,get_item,step4,search" {
		t.Errorf("step names = %s", got)
	}
	if f.Steps[2].ThinkMs == nil || *f.Steps[2].ThinkMs != 50 || f.Steps[2].Calls[0].As != "item" {
		t.Errorf("step 3 = %+v", f.Steps[2])
	}
	if fe := f.Steps[3].Calls[1].ForEach; fe["max"] != DefaultForEachMax || fe["path"] != "results" {
		t.Errorf("forEach = %v", fe)
	}
	if f.Weight != 2.5 || f.ThinkMs == nil || *f.ThinkMs != 0 {
		t.Errorf("flow = %+v", f)
	}
	b := f.Budgets
	if b.P95 != 1500 || *b.P99 != 3000 || b.Completion != 0.9 {
		t.Errorf("budgets = %+v", b)
	}
	// stepP95 applies to every step; the per-step entry wins for its step.
	if len(b.Steps) != 5 || *b.Steps["search-2"].P95 != 900 || *b.Steps["get_item"].P95 != 400 {
		t.Errorf("step budgets = %+v", b.Steps)
	}
	if w.Data["regions"].Pick != PickSequential || len(w.Data["regions"].Rows) != 2 {
		t.Errorf("regions = %+v", w.Data["regions"])
	}
	// Round trip through the WORKLOAD_FILE JSON.
	p := filepath.Join(t.TempDir(), "wl.json")
	if err := Write(p, w); err != nil {
		t.Fatal(err)
	}
	back, err := ReadNormalized(p)
	if err != nil {
		t.Fatal(err)
	}
	if back.Name != "shop" || back.Flows[0].Steps[1].Calls[0].Repeat != 1 {
		t.Errorf("round trip = %+v", back)
	}
	raw, _ := os.ReadFile(p)
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"args":{"n":3,"nested":{"list":[1,"{{data.regions}}"]},"region":"{{data.regions}}"}`) {
		t.Errorf("args JSON = %s", raw)
	}
}

func TestParseJSONAndBareWrapper(t *testing.T) {
	src := `{"name": "j", "flows": [{"name": "f", "weight": 1, "steps": ["fast"]}]}`
	w, err := Parse([]byte(src), ".")
	if err != nil {
		t.Fatal(err)
	}
	if w.Flows[0].Budgets.P95 != DefaultFlowP95Ms || w.Flows[0].Budgets.Completion != DefaultCompletion || w.Flows[0].Steps[0].Name != "fast" {
		t.Errorf("defaults = %+v", w.Flows[0])
	}
}

func TestParseErrors(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("c.csv", "id,email\nC-1,a@x\n")
	write("bad-header.csv", "id,e mail\nC-1,a@x\n")
	write("empty.csv", "id,email\n")
	write("ragged.csv", "id,email\nC-1\n")

	flow := func(steps string) string {
		return "workload:\n  name: w\n  data: { c: { file: c.csv } }\n  flows:\n    - name: f\n      weight: 1\n      steps:\n" + steps
	}
	cases := []struct{ name, src, want string }{
		{"empty", "", "the file is empty"},
		{"not yaml", "workload: [", "not valid YAML/JSON"},
		{"scalar", "hello", "must be a mapping"},
		{"unknown top key", "name: w\nflow: []\n", "workload: unknown key 'flow' (allowed:"},
		{"no name", "flows: []\n", "workload: name is required"},
		{"no flows", "name: w\n", "workload: flows must be a non-empty list"},
		{"agents", "name: w\nagents: 2.5\nflows: [{name: f, weight: 1, steps: [a]}]\n", "agents must be a whole number >= 1"},
		{"duration", "name: w\nduration: 10\nflows: [{name: f, weight: 1, steps: [a]}]\n", "duration must be a duration like 30s"},
		{"weight", "name: w\nflows: [{name: f, weight: 0, steps: [a]}]\n", "flow `f`: weight must be a number > 0"},
		{"flow name", "name: w\nflows: [{name: 'a b', weight: 1, steps: [a]}]\n", "flow `a b`: name is required and must be letters"},
		{"dup flow", "name: w\nflows: [{name: f, weight: 1, steps: [a]}, {name: f, weight: 1, steps: [a]}]\n", "flow 2: duplicate flow name `f`"},
		{"flow key", "name: w\nflows: [{name: f, weight: 1, step: [a]}]\n", "flow `f`: unknown key 'step'"},
		{"no steps", "name: w\nflows: [{name: f, weight: 1}]\n", "flow `f`: steps must be a non-empty list"},
		{"step key", flow("        - { name: s, tool: a, agrs: {} }\n"), "flow `f` step 1: unknown key 'agrs'"},
		{"no tool", flow("        - { name: s, args: {} }\n"), "flow `f` step 1 (s): tool is required"},
		{"parallel extra", flow("        - { parallel: [a], tool: b }\n"), "a parallel step takes name, thinkMs and parallel"},
		{"parallel call key", flow("        - { name: p, parallel: [a, { tool: b, nme: x }] }\n"), "flow `f` step 1 (p) call 2 (b): unknown key 'nme'"},
		{"dup step", flow("        - { name: s, tool: a }\n        - { name: s, tool: b }\n"), "flow `f` step 2: duplicate step name `s`"},
		{"step name", flow("        - { name: 'a b', tool: a }\n"), "flow `f` step 1: name must be letters"},
		{"ref unknown", flow("        - { tool: a, as: x }\n        - { name: s, tool: b, args: { q: { $from: y } } }\n"),
			"flow `f` step 2 (s, tool b) args.q: 'y' is not the `as` name of a call in an earlier step (known: x)"},
		{"ref same step", flow("        - parallel: [{ tool: a, as: x }, { tool: b, args: { q: { $from: x } } }]\n"),
			"call 2 (b) args.q: 'x' is not the `as` name of a call in an earlier step (no earlier step names a result)"},
		{"item outside forEach", flow("        - { tool: a, args: { q: { $from: $item } } }\n"), "$item is only available inside a call with forEach"},
		{"ref key", flow("        - { tool: a, as: x }\n        - { tool: b, args: { q: { $from: x, pth: a } } }\n"), "unknown key 'pth'"},
		{"repeat and forEach", flow("        - { tool: a, as: x }\n        - { tool: b, repeat: 2, forEach: { $from: x } }\n"), "use either repeat or forEach"},
		{"repeat", flow("        - { tool: a, repeat: 0 }\n"), "repeat must be a whole number >= 1"},
		{"dup as", flow("        - { tool: a, as: x }\n        - { tool: b, as: x }\n"), "duplicate as name 'x'"},
		{"template pool", flow("        - { tool: a, args: { q: '{{data.people.id}}' } }\n"), "args.q: {{data.people.id}} names data pool 'people', which `data:` does not define (pools: c)"},
		{"template column", flow("        - { tool: a, args: { q: 'x {{data.c.mail}}' } }\n"), "has no column 'mail' (columns: email, id)"},
		{"template form", flow("        - { tool: a, args: { q: '{{vu}}' } }\n"), "unknown template {{vu}}"},
		{"template needs column", flow("        - { tool: a, args: { q: '{{data.c}}' } }\n"), "{{data.c}} needs a column: data.c rows have email, id"},
		{"budget step", "name: w\nflows: [{name: f, weight: 1, steps: [a], budgets: {steps: {b: {p95: 1}}}}]\n", "flow `f` budgets.steps: no step named `b` (steps: a)"},
		{"budget value", "name: w\nflows: [{name: f, weight: 1, steps: [a], budgets: {p95: fast}}]\n", "flow `f` budgets.p95 must be a duration like 800ms or 3s"},
		{"budget completion", "name: w\nbudgets: {completion: 120%}\nflows: [{name: f, weight: 1, steps: [a]}]\n", "workload budgets.completion must be a fraction in (0, 1]"},
		{"budget p99 < p95", "name: w\nflows: [{name: f, weight: 1, steps: [a], budgets: {p95: 2s, p99: 1s}}]\n", "p99 (1000 ms) is below p95 (2000 ms)"},
		{"top steps budget", "name: w\nbudgets: {steps: {}}\nflows: [{name: f, weight: 1, steps: [a]}]\n", "workload budgets: unknown key 'steps'"},
		{"data both", "name: w\ndata: {c: {file: c.csv, values: [1]}}\nflows: [{name: f, weight: 1, steps: [a]}]\n", "data.c: set exactly one of file"},
		{"data pick", "name: w\ndata: {c: {values: [1], pick: roundrobin}}\nflows: [{name: f, weight: 1, steps: [a]}]\n", "data.c: pick must be random or sequential"},
		{"data missing file", "name: w\ndata: {c: {file: nope.csv}}\nflows: [{name: f, weight: 1, steps: [a]}]\n", "data.c: open"},
		{"data header", "name: w\ndata: {c: {file: bad-header.csv}}\nflows: [{name: f, weight: 1, steps: [a]}]\n", "header column 2 'e mail' must be letters"},
		{"data empty", "name: w\ndata: {c: {file: empty.csv}}\nflows: [{name: f, weight: 1, steps: [a]}]\n", "has a header but no rows"},
		{"data ragged", "name: w\ndata: {c: {file: ragged.csv}}\nflows: [{name: f, weight: 1, steps: [a]}]\n", "wrong number of fields"},
		{"data mixed", "name: w\ndata: {c: {values: [1, {a: 1}]}}\nflows: [{name: f, weight: 1, steps: [a]}]\n", "all mappings or all plain values"},
		{"data pool name", "name: w\ndata: {1c: {values: [1]}}\nflows: [{name: f, weight: 1, steps: [a]}]\n", "data.1c: pool names must be"},
		{"plain pool column", "name: w\ndata: {r: {values: [eu]}}\nflows: [{name: f, weight: 1, steps: [{tool: a, args: {q: '{{data.r.x}}'}}]}]\n", "data.r is a list of plain values (use {{data.r}})"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.src), dir)
			if err == nil {
				t.Fatalf("no error, want %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q\nwant it to contain %q", err, c.want)
			}
		})
	}
}

func TestLoadPrefixesFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "w.yaml")
	if err := os.WriteFile(p, []byte("name: w\nflows: [{name: f, weight: -1, steps: [a]}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil || !strings.HasPrefix(err.Error(), "workload "+p+": flow `f`: weight") {
		t.Errorf("err = %v", err)
	}
	if _, err := ReadNormalized(p); err == nil {
		t.Error("ReadNormalized accepted YAML")
	}
}
