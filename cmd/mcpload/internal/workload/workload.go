// Package workload loads workload profiles: a named mix of business flows
// (weighted multi-step tool plans), test data pools and budgets, written in
// YAML or JSON. Load validates a file and returns the normalized form that
// scenarios/workload.js runs; Write saves it as the JSON file the scenario
// reads from WORKLOAD_FILE.
//
// The file format (examples/workloads/ has a commented template):
//
//	workload:                      # optional wrapper; the mapping may also be the whole file
//	  name: customer-support
//	  description: ...
//	  agents: 20                   # VUs (--vus wins; default 10)
//	  duration: 2m                 # (--duration wins; default 2m)
//	  thinkMs: 500                 # mean pause between steps (default THINK_MS, 500)
//	  data:                        # test data pools, one row picked per agent session
//	    customers: { file: customers.csv, pick: random }   # CSV with a header row, relative to this file
//	    regions:   { values: [eu, us], pick: sequential }
//	  budgets:                     # defaults for every flow
//	    p95: 3s                    # flow end-to-end p95 (default 5000 ms); also p99
//	    completion: 99%            # minimum share of flows that complete (default 0.95)
//	    stepP95: 1s                # every step's p95 (default: none); also stepP99
//	  flows:
//	    - name: lookup-orders
//	      weight: 60
//	      budgets: { p95: 2s, steps: { get_orders: { p95: 800ms } } }
//	      steps:
//	        - search_customer                       # bare tool name: base args (TOOL_ARGS, demo or schema)
//	        - name: get_orders                      # a step with one call
//	          tool: search
//	          args: { query: "{{data.customers.email}}" }
//	          as: orders
//	        - name: details                         # a step with calls sent together
//	          parallel:
//	            - { tool: search, forEach: { $from: orders, path: results, max: 2 },
//	                args: { query: { $from: $item, path: title } } }
//	            - big
//
// Calls take the keys of scenarios/lib/workflow.js (tool, args, as, repeat,
// forEach, $from references); a step adds name and thinkMs. Strings in args
// may hold {{data.<pool>.<column>}} (or {{data.<pool>}} for a pool of plain
// values), filled from the session's row.
package workload

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults when a file sets no budget (the agent-workflow defaults).
const (
	DefaultFlowP95Ms     = 5000
	DefaultCompletion    = 0.95
	DefaultForEachMax    = 5
	PickRandom           = "random"
	PickSequential       = "sequential"
	itemRef              = "$item"
	maxDataRows          = 1_000_000
	defaultParallelStepN = "step"
)

var (
	// Flow and step names become k6 tag values in threshold selectors.
	nameRe     = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	poolRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	columnRe   = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	templateRe = regexp.MustCompile(`\{\{\s*(.*?)\s*\}\}`)
	dataRefRe  = regexp.MustCompile(`^data\.([A-Za-z_][A-Za-z0-9_]*)(?:\.([A-Za-z0-9_]+))?$`)
	unsafeRe   = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
)

// Workload is the normalized profile handed to scenarios/workload.js.
type Workload struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Agents      int              `json:"agents,omitempty"`
	Duration    string           `json:"duration,omitempty"`
	ThinkMs     *float64         `json:"thinkMs,omitempty"`
	Data        map[string]*Pool `json:"data"`
	Flows       []Flow           `json:"flows"`
}

// Pool is a data pool: Rows are objects (CSV rows, column -> string) or plain values.
type Pool struct {
	Pick   string `json:"pick"`
	Source string `json:"source,omitempty"` // the CSV file, for messages
	Rows   []any  `json:"rows"`
}

// Flow is one weighted business flow.
type Flow struct {
	Name    string   `json:"name"`
	Weight  float64  `json:"weight"`
	ThinkMs *float64 `json:"thinkMs,omitempty"`
	Budgets Budgets  `json:"budgets"`
	Steps   []Step   `json:"steps"`
}

// Budgets are a flow's resolved budgets: end-to-end P95 (always set), P99,
// the minimum completion rate, and per-step latency budgets (steps without
// one are absent).
type Budgets struct {
	P95        float64               `json:"p95"`
	P99        *float64              `json:"p99,omitempty"`
	Completion float64               `json:"completion"`
	Steps      map[string]StepBudget `json:"steps,omitempty"`
}

// StepBudget is one step's latency budget in ms.
type StepBudget struct {
	P95 *float64 `json:"p95,omitempty"`
	P99 *float64 `json:"p99,omitempty"`
}

// Step is a scenarios/lib/workflow.js step: its calls are sent together.
type Step struct {
	Name    string   `json:"name"`
	ThinkMs *float64 `json:"thinkMs,omitempty"`
	Calls   []Call   `json:"calls"`
}

// Call is a scenarios/lib/workflow.js call.
type Call struct {
	Tool    string         `json:"tool"`
	Args    map[string]any `json:"args"`
	As      string         `json:"as,omitempty"`
	Repeat  int            `json:"repeat"`
	ForEach map[string]any `json:"forEach,omitempty"`
}

// Load reads and validates a workload file (YAML or JSON). Data files are
// resolved relative to the workload file. Errors name the flow, step and
// field at fault.
func Load(path string) (*Workload, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	w, err := Parse(b, filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("workload %s: %w", path, err)
	}
	return w, nil
}

// Parse validates a workload document; dir is where relative data files live.
func Parse(b []byte, dir string) (*Workload, error) {
	var doc any
	dec := yaml.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the file is empty")
		}
		return nil, fmt.Errorf("not valid YAML/JSON: %w", err)
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("the file must be a mapping with a `workload:` key (or the workload's fields at the top level)")
	}
	if inner, ok := root["workload"]; ok && len(root) == 1 {
		if root, ok = inner.(map[string]any); !ok {
			return nil, errors.New("`workload` must be a mapping")
		}
	}
	p := &parser{dir: dir}
	return p.workload(root)
}

type parser struct{ dir string }

func keys(m map[string]any, where string, allowed ...string) error {
	var bad []string
	for k := range m {
		if !contains(allowed, k) {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("%s: unknown key '%s' (allowed: %s)", where, strings.Join(bad, "', '"), strings.Join(allowed, ", "))
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	}
	return 0, false
}

// millis reads a latency: a number of ms, or a duration string like "800ms" or "3s".
func millis(v any, where string) (float64, error) {
	if f, ok := num(v); ok {
		if f <= 0 {
			return 0, fmt.Errorf("%s must be > 0 (got %v)", where, v)
		}
		return f, nil
	}
	if s, ok := v.(string); ok {
		s = strings.TrimSpace(s)
		if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
			return f, nil
		}
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return float64(d) / float64(time.Millisecond), nil
		}
	}
	return 0, fmt.Errorf("%s must be a duration like 800ms or 3s, or a number of ms (got %v)", where, v)
}

// rate reads a completion rate: a number in (0, 1] or a percentage like "99%".
func rate(v any, where string) (float64, error) {
	f, ok := num(v)
	if s, isStr := v.(string); isStr && strings.HasSuffix(strings.TrimSpace(s), "%") {
		p, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%")), 64)
		f, ok = p/100, err == nil
	}
	if !ok || f <= 0 || f > 1 {
		return 0, fmt.Errorf("%s must be a fraction in (0, 1] or a percentage like 99%% (got %v)", where, v)
	}
	return f, nil
}

func thinkMs(v any, where string) (*float64, error) {
	if v == nil {
		return nil, nil
	}
	f, ok := num(v)
	if !ok || f < 0 {
		return nil, fmt.Errorf("%s: thinkMs must be a number of ms >= 0 (got %v)", where, v)
	}
	return &f, nil
}

func (p *parser) workload(m map[string]any) (*Workload, error) {
	if err := keys(m, "workload", "name", "description", "agents", "duration", "thinkMs", "data", "budgets", "flows"); err != nil {
		return nil, err
	}
	w := &Workload{Data: map[string]*Pool{}}
	name, ok := m["name"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return nil, errors.New("workload: name is required (a short label, e.g. customer-support)")
	}
	w.Name = strings.TrimSpace(name)
	if d, ok := m["description"]; ok {
		s, isStr := d.(string)
		if !isStr {
			return nil, errors.New("workload: description must be a string")
		}
		w.Description = strings.TrimSpace(s)
	}
	if a, ok := m["agents"]; ok {
		f, isNum := num(a)
		if !isNum || f < 1 || f != math.Trunc(f) {
			return nil, fmt.Errorf("workload: agents must be a whole number >= 1 (got %v)", a)
		}
		w.Agents = int(f)
	}
	if d, ok := m["duration"]; ok {
		s, isStr := d.(string)
		if dd, err := time.ParseDuration(strings.TrimSpace(s)); !isStr || err != nil || dd <= 0 {
			return nil, fmt.Errorf("workload: duration must be a duration like 30s, 2m or 1h30m (got %v)", d)
		}
		w.Duration = strings.TrimSpace(s)
	}
	var err error
	if w.ThinkMs, err = thinkMs(m["thinkMs"], "workload"); err != nil {
		return nil, err
	}
	if d, ok := m["data"]; ok && d != nil {
		dm, isMap := d.(map[string]any)
		if !isMap {
			return nil, errors.New("workload: data must be a mapping of pool name -> {file: x.csv} or {values: [...]}")
		}
		for _, name := range sortedKeys(dm) {
			if w.Data[name], err = p.pool(name, dm[name]); err != nil {
				return nil, err
			}
		}
	}
	def, err := budgetsIn(m["budgets"], "workload budgets", false)
	if err != nil {
		return nil, err
	}
	fl, ok := m["flows"].([]any)
	if !ok || len(fl) == 0 {
		return nil, errors.New("workload: flows must be a non-empty list")
	}
	seen := map[string]bool{}
	for i, f := range fl {
		flow, err := p.flow(i, f, def, w.Data)
		if err != nil {
			return nil, err
		}
		if seen[flow.Name] {
			return nil, fmt.Errorf("flow %d: duplicate flow name `%s`", i+1, flow.Name)
		}
		seen[flow.Name] = true
		w.Flows = append(w.Flows, flow)
	}
	return w, nil
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (p *parser) pool(name string, v any) (*Pool, error) {
	where := fmt.Sprintf("data.%s", name)
	if !poolRe.MatchString(name) {
		return nil, fmt.Errorf("%s: pool names must be letters, digits and _ (not starting with a digit)", where)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a mapping: {file: x.csv} or {values: [...]}", where)
	}
	if err := keys(m, where, "file", "values", "pick"); err != nil {
		return nil, err
	}
	pl := &Pool{Pick: PickRandom}
	if pk, ok := m["pick"]; ok {
		s, _ := pk.(string)
		if s != PickRandom && s != PickSequential {
			return nil, fmt.Errorf("%s: pick must be random or sequential (got %v)", where, pk)
		}
		pl.Pick = s
	}
	file, hasFile := m["file"]
	vals, hasVals := m["values"]
	switch {
	case hasFile == hasVals:
		return nil, fmt.Errorf("%s: set exactly one of file (a CSV file) or values (a list)", where)
	case hasFile:
		f, ok := file.(string)
		if !ok || f == "" {
			return nil, fmt.Errorf("%s: file must be a path", where)
		}
		if !filepath.IsAbs(f) {
			f = filepath.Join(p.dir, f)
		}
		rows, err := readCSV(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		pl.Source, pl.Rows = f, rows
	default:
		list, ok := vals.([]any)
		if !ok || len(list) == 0 {
			return nil, fmt.Errorf("%s: values must be a non-empty list", where)
		}
		objects := 0
		for i, x := range list {
			clean, err := jsonValue(x, fmt.Sprintf("%s.values[%d]", where, i))
			if err != nil {
				return nil, err
			}
			if obj, ok := clean.(map[string]any); ok {
				objects++
				for k := range obj {
					if !columnRe.MatchString(k) {
						return nil, fmt.Errorf("%s.values[%d]: key '%s' must be letters, digits and _", where, i, k)
					}
				}
			}
			list[i] = clean
		}
		if objects != 0 && objects != len(list) {
			return nil, fmt.Errorf("%s: values must be all mappings or all plain values, not a mix", where)
		}
		pl.Rows = list
	}
	return pl, nil
}

// readCSV reads a CSV with a header row into objects (column -> string).
func readCSV(path string) ([]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.TrimLeadingSpace = true
	header, err := r.Read()
	if err == io.EOF {
		return nil, fmt.Errorf("%s is empty (it needs a header row and at least one row)", path)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(header) > 0 {
		header[0] = strings.TrimPrefix(header[0], string(rune(0xFEFF))) // a UTF-8 BOM from spreadsheet exports
	}
	for i, h := range header {
		header[i] = strings.TrimSpace(h)
		if !columnRe.MatchString(header[i]) {
			return nil, fmt.Errorf("%s: header column %d '%s' must be letters, digits and _ so {{data.<pool>.<column>}} can name it", path, i+1, h)
		}
		for _, prev := range header[:i] {
			if prev == header[i] {
				return nil, fmt.Errorf("%s: duplicate header column '%s'", path, header[i])
			}
		}
	}
	var rows []any
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		row := make(map[string]any, len(header))
		for i, h := range header {
			row[h] = rec[i]
		}
		rows = append(rows, row)
		if len(rows) > maxDataRows {
			return nil, fmt.Errorf("%s: more than %d rows", path, maxDataRows)
		}
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s has a header but no rows", path)
	}
	return rows, nil
}

// jsonValue checks that a decoded YAML value can be sent as JSON (string keys only).
func jsonValue(v any, where string) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			c, err := jsonValue(e, where+"."+k)
			if err != nil {
				return nil, err
			}
			x[k] = c
		}
		return x, nil
	case map[any]any:
		return nil, fmt.Errorf("%s: mapping keys must be strings", where)
	case []any:
		for i, e := range x {
			c, err := jsonValue(e, fmt.Sprintf("%s[%d]", where, i))
			if err != nil {
				return nil, err
			}
			x[i] = c
		}
		return x, nil
	case time.Time:
		// YAML reads an unquoted 2026-01-02 as a timestamp; send it back as written.
		if x.Hour() == 0 && x.Minute() == 0 && x.Second() == 0 && x.Nanosecond() == 0 {
			return x.Format("2006-01-02"), nil
		}
		return x.Format(time.RFC3339Nano), nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, fmt.Errorf("%s: %v is not a JSON number", where, x)
		}
	}
	return v, nil
}

// rawBudgets is one `budgets` block before defaults are applied.
type rawBudgets struct {
	p95, p99, completion, stepP95, stepP99 *float64
	steps                                  map[string]StepBudget
	stepsAt                                []string // step names in file order, for messages
}

func budgetsIn(v any, where string, perFlow bool) (rawBudgets, error) {
	var b rawBudgets
	if v == nil {
		return b, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return b, fmt.Errorf("%s must be a mapping", where)
	}
	allowed := []string{"p95", "p99", "completion", "stepP95", "stepP99"}
	if perFlow {
		allowed = append(allowed, "steps")
	}
	if err := keys(m, where, allowed...); err != nil {
		return b, err
	}
	lat := func(k string) (*float64, error) {
		x, ok := m[k]
		if !ok {
			return nil, nil
		}
		f, err := millis(x, where+"."+k)
		return &f, err
	}
	var err error
	if b.p95, err = lat("p95"); err != nil {
		return b, err
	}
	if b.p99, err = lat("p99"); err != nil {
		return b, err
	}
	if b.stepP95, err = lat("stepP95"); err != nil {
		return b, err
	}
	if b.stepP99, err = lat("stepP99"); err != nil {
		return b, err
	}
	if c, ok := m["completion"]; ok {
		f, err := rate(c, where+".completion")
		if err != nil {
			return b, err
		}
		b.completion = &f
	}
	if s, ok := m["steps"]; ok {
		sm, isMap := s.(map[string]any)
		if !isMap {
			return b, fmt.Errorf("%s.steps must be a mapping of step name -> {p95, p99}", where)
		}
		b.steps = map[string]StepBudget{}
		for _, name := range sortedKeys(sm) {
			sw := fmt.Sprintf("%s.steps.%s", where, name)
			e, ok := sm[name].(map[string]any)
			if !ok {
				return b, fmt.Errorf("%s must be a mapping {p95, p99}", sw)
			}
			if err := keys(e, sw, "p95", "p99"); err != nil {
				return b, err
			}
			var sb StepBudget
			for _, k := range []string{"p95", "p99"} {
				if x, ok := e[k]; ok {
					f, err := millis(x, sw+"."+k)
					if err != nil {
						return b, err
					}
					if k == "p95" {
						sb.P95 = &f
					} else {
						sb.P99 = &f
					}
				}
			}
			b.steps[name] = sb
			b.stepsAt = append(b.stepsAt, name)
		}
	}
	return b, nil
}

func first(vs ...*float64) *float64 {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}

func (p *parser) flow(i int, v any, def rawBudgets, data map[string]*Pool) (Flow, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return Flow{}, fmt.Errorf("flow %d must be a mapping with name, weight and steps", i+1)
	}
	where := fmt.Sprintf("flow %d", i+1)
	if n, ok := m["name"].(string); ok && n != "" {
		where = fmt.Sprintf("flow `%s`", n)
	}
	if err := keys(m, where, "name", "weight", "thinkMs", "budgets", "steps"); err != nil {
		return Flow{}, err
	}
	name, _ := m["name"].(string)
	if !nameRe.MatchString(name) {
		return Flow{}, fmt.Errorf("%s: name is required and must be letters, digits, '_', '.' or '-' (got %q)", where, name)
	}
	f := Flow{Name: name}
	wt, ok := num(m["weight"])
	if !ok || wt <= 0 {
		return Flow{}, fmt.Errorf("%s: weight must be a number > 0 (got %v; remove the flow to disable it)", where, m["weight"])
	}
	f.Weight = wt
	var err error
	if f.ThinkMs, err = thinkMs(m["thinkMs"], where); err != nil {
		return Flow{}, err
	}
	steps, ok := m["steps"].([]any)
	if !ok || len(steps) == 0 {
		return Flow{}, fmt.Errorf("%s: steps must be a non-empty list", where)
	}
	if f.Steps, err = p.steps(where, steps, data); err != nil {
		return Flow{}, err
	}
	fb, err := budgetsIn(m["budgets"], where+" budgets", true)
	if err != nil {
		return Flow{}, err
	}
	b := Budgets{P95: DefaultFlowP95Ms, Completion: DefaultCompletion}
	if v := first(fb.p95, def.p95); v != nil {
		b.P95 = *v
	}
	b.P99 = first(fb.p99, def.p99)
	if v := first(fb.completion, def.completion); v != nil {
		b.Completion = *v
	}
	if b.P99 != nil && *b.P99 < b.P95 {
		return Flow{}, fmt.Errorf("%s budgets: p99 (%v ms) is below p95 (%v ms)", where, *b.P99, b.P95)
	}
	for _, n := range fb.stepsAt {
		if !hasStep(f.Steps, n) {
			return Flow{}, fmt.Errorf("%s budgets.steps: no step named `%s` (steps: %s)", where, n, stepNames(f.Steps))
		}
	}
	for _, st := range f.Steps {
		own := fb.steps[st.Name]
		sb := StepBudget{P95: first(own.P95, fb.stepP95, def.stepP95), P99: first(own.P99, fb.stepP99, def.stepP99)}
		if sb.P95 == nil && sb.P99 == nil {
			continue
		}
		if b.Steps == nil {
			b.Steps = map[string]StepBudget{}
		}
		b.Steps[st.Name] = sb
	}
	f.Budgets = b
	return f, nil
}

func hasStep(steps []Step, name string) bool {
	for _, s := range steps {
		if s.Name == name {
			return true
		}
	}
	return false
}

func stepNames(steps []Step) string {
	var n []string
	for _, s := range steps {
		n = append(n, s.Name)
	}
	return strings.Join(n, ", ")
}

func (p *parser) steps(flowWhere string, raw []any, data map[string]*Pool) ([]Step, error) {
	// Explicit names first, so a default name never takes one a later step asked for.
	used := map[string]bool{}
	for i, s := range raw {
		if m, ok := s.(map[string]any); ok {
			if n, ok := m["name"]; ok {
				ns, _ := n.(string)
				where := fmt.Sprintf("%s step %d", flowWhere, i+1)
				if !nameRe.MatchString(ns) {
					return nil, fmt.Errorf("%s: name must be letters, digits, '_', '.' or '-' (got %q)", where, n)
				}
				if used[ns] {
					return nil, fmt.Errorf("%s: duplicate step name `%s`", where, ns)
				}
				used[ns] = true
			}
		}
	}
	unique := func(base string) string {
		n := base
		for k := 2; used[n]; k++ {
			n = fmt.Sprintf("%s-%d", base, k)
		}
		used[n] = true
		return n
	}
	var known []string // `as` names of earlier steps
	var out []Step
	for i, s := range raw {
		where := fmt.Sprintf("%s step %d", flowWhere, i+1)
		var st Step
		var callsRaw []any
		switch x := s.(type) {
		case string:
			callsRaw = []any{x}
		case map[string]any:
			if _, par := x["parallel"]; par {
				if err := keys(x, where, "name", "thinkMs", "parallel"); err != nil {
					return nil, fmt.Errorf("%s (a parallel step takes name, thinkMs and parallel; put tool, args and as on each call)", err)
				}
				list, ok := x["parallel"].([]any)
				if !ok || len(list) == 0 {
					return nil, fmt.Errorf("%s: parallel must be a non-empty list of calls", where)
				}
				callsRaw = list
			} else {
				if err := keys(x, where, "name", "thinkMs", "tool", "args", "as", "repeat", "forEach"); err != nil {
					return nil, err
				}
				call := map[string]any{}
				for k, v := range x {
					if k != "name" && k != "thinkMs" {
						call[k] = v
					}
				}
				callsRaw = []any{call}
			}
			var err error
			if st.ThinkMs, err = thinkMs(x["thinkMs"], where); err != nil {
				return nil, err
			}
			if n, ok := x["name"].(string); ok {
				st.Name = n
				where = fmt.Sprintf("%s step %d (%s)", flowWhere, i+1, n)
			}
		default:
			return nil, fmt.Errorf("%s must be a tool name or a mapping (tool/args/as, or parallel: [...])", where)
		}
		var added []string
		for j, c := range callsRaw {
			cw := where
			if len(callsRaw) > 1 {
				cw = fmt.Sprintf("%s call %d", where, j+1)
			}
			call, err := callIn(c, cw, known, data)
			if err != nil {
				return nil, err
			}
			if call.As != "" {
				if contains(known, call.As) || contains(added, call.As) {
					return nil, fmt.Errorf("%s: duplicate as name '%s'", cw, call.As)
				}
				added = append(added, call.As)
			}
			st.Calls = append(st.Calls, call)
		}
		// Names defined in this step only become usable in later steps: its calls run together.
		known = append(known, added...)
		if st.Name == "" {
			base := defaultParallelStepN + strconv.Itoa(i+1)
			if len(st.Calls) == 1 {
				base = unsafeRe.ReplaceAllString(st.Calls[0].Tool, "_")
			}
			st.Name = unique(base)
		}
		out = append(out, st)
	}
	return out, nil
}

func callIn(v any, where string, known []string, data map[string]*Pool) (Call, error) {
	var m map[string]any
	switch x := v.(type) {
	case string:
		m = map[string]any{"tool": x}
	case map[string]any:
		m = x
	default:
		return Call{}, fmt.Errorf("%s: a call must be a tool name or a mapping {tool, args, as, repeat, forEach}", where)
	}
	tool, _ := m["tool"].(string)
	switch {
	case tool == "" || strings.Contains(where, "("+tool+")"):
	case strings.HasSuffix(where, ")"): // "step 2 (get_orders)" -> "step 2 (get_orders, tool search)"
		where = fmt.Sprintf("%s, tool %s)", where[:len(where)-1], tool)
	default:
		where = fmt.Sprintf("%s (%s)", where, tool)
	}
	if err := keys(m, where, "tool", "args", "as", "repeat", "forEach"); err != nil {
		return Call{}, err
	}
	if strings.TrimSpace(tool) == "" {
		return Call{}, fmt.Errorf("%s: tool is required (the MCP tool name)", where)
	}
	c := Call{Tool: tool, Args: map[string]any{}, Repeat: 1}
	if a, ok := m["args"]; ok && a != nil {
		am, isMap := a.(map[string]any)
		if !isMap {
			return Call{}, fmt.Errorf("%s: args must be a mapping", where)
		}
		clean, err := jsonValue(am, where+" args")
		if err != nil {
			return Call{}, err
		}
		c.Args = clean.(map[string]any)
	}
	if r, ok := m["repeat"]; ok {
		f, isNum := num(r)
		if !isNum || f < 1 || f != math.Trunc(f) {
			return Call{}, fmt.Errorf("%s: repeat must be a whole number >= 1 (got %v)", where, r)
		}
		c.Repeat = int(f)
	}
	if fe, ok := m["forEach"]; ok {
		ref, isMap := fe.(map[string]any)
		if !isMap || ref["$from"] == nil {
			return Call{}, fmt.Errorf("%s: forEach must be a reference {$from: <as name>, path, max}", where)
		}
		if c.Repeat != 1 {
			return Call{}, fmt.Errorf("%s: use either repeat or forEach, not both", where)
		}
		if err := checkRef(ref, where+" forEach", known, false, true); err != nil {
			return Call{}, err
		}
		out := map[string]any{}
		for k, v := range ref {
			out[k] = v
		}
		if _, ok := out["max"]; !ok {
			out["max"] = DefaultForEachMax
		}
		c.ForEach = out
	}
	if err := checkArgs(c.Args, where+" args", known, c.ForEach != nil, data); err != nil {
		return Call{}, err
	}
	if a, ok := m["as"]; ok {
		s, isStr := a.(string)
		if !isStr || s == "" || s == itemRef {
			return Call{}, fmt.Errorf("%s: as must be a non-empty name other than $item", where)
		}
		c.As = s
	}
	return c, nil
}

func checkRef(ref map[string]any, where string, known []string, allowItem, allowMax bool) error {
	from, ok := ref["$from"].(string)
	if !ok || from == "" {
		return fmt.Errorf("%s: $from must be the `as` name of a call in an earlier step", where)
	}
	allowed := []string{"$from", "path", "match"}
	if allowMax {
		allowed = append(allowed, "max")
	}
	if err := keys(ref, where, allowed...); err != nil {
		return err
	}
	for _, k := range []string{"path", "match"} {
		if v, ok := ref[k]; ok {
			if _, isStr := v.(string); !isStr {
				return fmt.Errorf("%s: %s must be a string", where, k)
			}
		}
	}
	if mx, ok := ref["max"]; ok {
		f, isNum := num(mx)
		if !isNum || f < 1 || f != math.Trunc(f) {
			return fmt.Errorf("%s: max must be a whole number >= 1 (got %v)", where, mx)
		}
	}
	if from == itemRef {
		if !allowItem {
			return fmt.Errorf("%s: $item is only available inside a call with forEach", where)
		}
		return nil
	}
	if !contains(known, from) {
		if len(known) == 0 {
			return fmt.Errorf("%s: '%s' is not the `as` name of a call in an earlier step (no earlier step names a result)", where, from)
		}
		return fmt.Errorf("%s: '%s' is not the `as` name of a call in an earlier step (known: %s)", where, from, strings.Join(known, ", "))
	}
	return nil
}

// checkArgs validates references and {{data...}} templates anywhere in args.
func checkArgs(v any, where string, known []string, allowItem bool, data map[string]*Pool) error {
	switch x := v.(type) {
	case map[string]any:
		if _, isRef := x["$from"]; isRef {
			return checkRef(x, where, known, allowItem, false)
		}
		for _, k := range sortedKeys(x) {
			if err := checkArgs(x[k], where+"."+k, known, allowItem, data); err != nil {
				return err
			}
		}
	case []any:
		for i, e := range x {
			if err := checkArgs(e, fmt.Sprintf("%s[%d]", where, i), known, allowItem, data); err != nil {
				return err
			}
		}
	case string:
		return checkTemplates(x, where, data)
	}
	return nil
}

func checkTemplates(s, where string, data map[string]*Pool) error {
	for _, m := range templateRe.FindAllStringSubmatch(s, -1) {
		ref := dataRefRe.FindStringSubmatch(m[1])
		if ref == nil {
			return fmt.Errorf("%s: unknown template %s (use {{data.<pool>.<column>}}, or {{data.<pool>}} for a list of plain values)", where, m[0])
		}
		pool, col := ref[1], ref[2]
		pl := data[pool]
		if pl == nil {
			return fmt.Errorf("%s: %s names data pool '%s', which `data:` does not define%s", where, m[0], pool, poolList(data))
		}
		_, objects := pl.Rows[0].(map[string]any)
		switch {
		case objects && col == "":
			return fmt.Errorf("%s: %s needs a column: data.%s rows have %s", where, m[0], pool, columns(pl))
		case !objects && col != "":
			return fmt.Errorf("%s: %s names a column, but data.%s is a list of plain values (use {{data.%s}})", where, m[0], pool, pool)
		case objects:
			for i, r := range pl.Rows {
				if _, ok := r.(map[string]any)[col]; !ok {
					if pl.Source != "" {
						return fmt.Errorf("%s: %s: %s has no column '%s' (columns: %s)", where, m[0], pl.Source, col, columns(pl))
					}
					return fmt.Errorf("%s: %s: data.%s row %d has no key '%s'", where, m[0], pool, i+1, col)
				}
			}
		}
	}
	return nil
}

func poolList(data map[string]*Pool) string {
	if len(data) == 0 {
		return ""
	}
	var n []string
	for k := range data {
		n = append(n, k)
	}
	sort.Strings(n)
	return " (pools: " + strings.Join(n, ", ") + ")"
}

func columns(pl *Pool) string {
	r, _ := pl.Rows[0].(map[string]any)
	var c []string
	for k := range r {
		c = append(c, k)
	}
	sort.Strings(c)
	return strings.Join(c, ", ")
}

// ToolNames lists the tools a workload calls, in order of first use.
func (w *Workload) ToolNames() []string {
	var out []string
	for _, f := range w.Flows {
		for _, s := range f.Steps {
			for _, c := range s.Calls {
				if !contains(out, c.Tool) {
					out = append(out, c.Tool)
				}
			}
		}
	}
	return out
}

// Write saves the normalized workload as JSON (the WORKLOAD_FILE format).
func Write(path string, w *Workload) error {
	b, err := json.Marshal(w)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// ReadNormalized reads a file written by Write (the report needs the flow
// weights and budgets, which the k6 metrics do not carry).
func ReadNormalized(path string) (*Workload, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var w Workload
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if w.Name == "" || len(w.Flows) == 0 {
		return nil, fmt.Errorf("%s: not a normalized workload (no name or flows)", path)
	}
	return &w, nil
}
