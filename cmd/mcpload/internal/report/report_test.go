package report

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const repoRoot = "../../../../"

func examplePath(name string) string { return filepath.Join(repoRoot, "report", "examples", name) }

func TestEmbeddedTemplateInSync(t *testing.T) {
	src := filepath.Join(repoRoot, "report", "template", "report.html")
	b, err := os.ReadFile(src)
	if os.IsNotExist(err) {
		t.Skip("report/template/report.html not present")
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != TemplateHTML() {
		t.Fatalf("internal/report/template.html differs from %s; copy it over", src)
	}
}

func TestRoundTripValidates(t *testing.T) {
	dir := t.TempDir()
	var outs []string
	for _, name := range []string{"healthy.json", "leaky.json"} {
		r, err := ReadJSON(examplePath(name))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Check(); err != nil {
			t.Fatalf("%s: Check: %v", name, err)
		}
		out := filepath.Join(dir, name)
		if err := WriteJSON(out, r); err != nil {
			t.Fatal(err)
		}
		r2, err := ReadJSON(out)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(r)
		b, _ := json.Marshal(r2)
		if !bytes.Equal(a, b) {
			t.Fatalf("%s: round trip changed content", name)
		}
		outs = append(outs, out)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found; skipping validate.mjs")
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "report", "node_modules", "ajv")); err != nil {
		t.Skip("report/node_modules not installed; skipping validate.mjs")
	}
	cmd := exec.Command(node, append([]string{filepath.Join(repoRoot, "report", "validate.mjs")}, outs...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("validate.mjs failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

func TestCheckCatchesErrors(t *testing.T) {
	r, err := ReadJSON(examplePath("healthy.json"))
	if err != nil {
		t.Fatal(err)
	}
	r.Series.Client.RPS = r.Series.Client.RPS[1:]
	r.Phases.LoadEndS = r.Phases.CooldownEndS + 1
	r.Summary.Errors++
	r.Tools[0].P50 = r.Tools[0].Max + 1
	err = r.Check()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"client.rps", "phases", "byErrorType", "percentiles"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestNormalizeEmptyReportValidates(t *testing.T) {
	r := &Report{
		Tool:   ToolInfo{Name: "mcpload", Version: "0.0.0"},
		Run:    Run{ID: "x", StartedAt: "2026-01-01T00:00:00Z", EndedAt: "2026-01-01T00:00:01Z", Scenario: "soak", Protocol: "2025-06-18", Target: Target{URL: "http://localhost/mcp"}, K6Version: "v1", Load: Load{Executor: "constant-vus"}},
		Series: Series{IntervalS: 1},
	}
	b, err := Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Check(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"byErrorType": {}`, `"tools": []`, `"rssBytes": []`, `"sampler": "none"`} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("missing %s in\n%s", want, b)
		}
	}
}

func TestPassed(t *testing.T) {
	h, _ := ReadJSON(examplePath("healthy.json"))
	l, _ := ReadJSON(examplePath("leaky.json"))
	if !Passed(h) {
		t.Error("healthy should pass")
	}
	if Passed(l) {
		t.Error("leaky should fail")
	}
	h.Thresholds[0].Passed = false
	if Passed(h) {
		t.Error("failed threshold should fail")
	}
}

func TestRenderHTML(t *testing.T) {
	r, err := ReadJSON(examplePath("leaky.json"))
	if err != nil {
		t.Fatal(err)
	}
	r.Run.Target.Label = "a</script><b>&"
	var buf bytes.Buffer
	if err := RenderHTML(r, &buf); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	m := regexp.MustCompile(`(?s)<script type="application/json" id="report-data">(.*?)</script>`).FindStringSubmatch(html)
	if m == nil {
		t.Fatal("no report-data script")
	}
	var back Report
	if err := json.Unmarshal([]byte(m[1]), &back); err != nil {
		t.Fatalf("embedded JSON invalid: %v", err)
	}
	if back.Run.ID != r.Run.ID || back.Run.Target.Label != r.Run.Target.Label {
		t.Fatal("embedded JSON mismatch")
	}
	if strings.Contains(m[1], "<") || strings.Contains(m[1], ">") || strings.Contains(m[1], "&") {
		t.Fatal("unescaped <>& in embedded JSON")
	}
	if !strings.Contains(html, "<title>mcpload · a/scriptb · soak</title>") {
		t.Fatalf("bad title: %s", regexp.MustCompile(`<title>.*</title>`).FindString(html))
	}
}
