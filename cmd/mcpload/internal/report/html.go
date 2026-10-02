package report

import (
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
)

// templateHTML is a verbatim copy of report/template/report.html
// (TestEmbeddedTemplateInSync fails when they diverge).
//
//go:embed template.html
var templateHTML string

// TemplateHTML returns the embedded HTML template.
func TemplateHTML() string { return templateHTML }

var (
	reDataScript = regexp.MustCompile(`(<script type="application/json" id="report-data">)((?s:.*?))(</script>)`)
	reTitle      = regexp.MustCompile(`<title>[^<]*</title>`)
)

// RenderHTML writes the self-contained HTML report for r, injecting the JSON
// exactly as report/render.mjs does: compact JSON with <, > and & escaped as
// <, >, & inside <script type="application/json" id="report-data">
// (first occurrence only), and <title> set to "mcpload · <label|url> · <scenario>"
// with <, >, & stripped. r is normalized first; it is not validated.
func RenderHTML(r *Report, w io.Writer) error {
	r.Normalize()
	// encoding/json escapes <, >, & (and U+2028/2029) as \u00XX by default.
	js, err := json.Marshal(r)
	if err != nil {
		return err
	}
	loc := reDataScript.FindStringSubmatchIndex(templateHTML)
	if loc == nil {
		return errors.New(`template is missing <script type="application/json" id="report-data">`)
	}
	// loc[4]:loc[5] is the old script content.
	html := templateHTML[:loc[4]] + string(js) + templateHTML[loc[5]:]

	label := r.Run.Target.Label
	if label == "" {
		label = r.Run.Target.URL
	}
	if label == "" {
		label = "report"
	}
	title := strings.NewReplacer("<", "", ">", "", "&", "").Replace("mcpload · " + label + " · " + r.Run.Scenario)
	if tl := reTitle.FindStringIndex(html); tl != nil {
		html = html[:tl[0]] + "<title>" + title + "</title>" + html[tl[1]:]
	}
	_, err = io.WriteString(w, html)
	return err
}
