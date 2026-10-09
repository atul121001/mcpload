package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestTargetDisplay(t *testing.T) {
	cases := []struct {
		t     Target
		want  string
		stdio bool
	}{
		{Target{URL: "http://h/mcp"}, "http://h/mcp", false},
		{Target{URL: "http://h/mcp", Transport: TransportHTTP}, "http://h/mcp", false},
		{Target{Command: []string{"node", "server.mjs"}, Transport: TransportStdio}, "node server.mjs", true},
		{Target{Command: []string{"node", "my server.js", "", `C:\x`, "it's"}}, `node 'my server.js' '' 'C:\x' "it's"`, true},
	}
	for _, c := range cases {
		if got := c.t.Display(); got != c.want {
			t.Errorf("%+v: Display %q, want %q", c.t, got, c.want)
		}
		if c.t.IsStdio() != c.stdio {
			t.Errorf("%+v: IsStdio %v", c.t, !c.stdio)
		}
	}
}

func TestStdioTargetJSONAndHTML(t *testing.T) {
	r, err := ReadJSON(examplePath("healthy.json"))
	if err != nil {
		t.Fatal(err)
	}
	r.Run.Target = Target{Command: []string{"node", "server.mjs"}, Transport: TransportStdio}
	r.Series.Server.Sampler = SamplerProcess
	if err := r.Check(); err != nil {
		t.Fatal(err)
	}
	b, err := Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Run struct {
			Target map[string]any `json:"target"`
		} `json:"run"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw.Run.Target["url"]; ok {
		t.Errorf("url written for a stdio target: %v", raw.Run.Target)
	}
	if raw.Run.Target["transport"] != "stdio" {
		t.Errorf("target %v", raw.Run.Target)
	}
	var html bytes.Buffer
	if err := RenderHTML(r, &html); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html.String(), "<title>mcpload · node server.mjs · "+r.Run.Scenario+"</title>") {
		t.Error("title does not name the command")
	}
}
