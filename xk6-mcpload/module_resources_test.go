package mcpload

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.k6.io/k6/v2/js/modulestest"
	"go.k6.io/k6/v2/lib"
	"go.k6.io/k6/v2/metrics"
)

// featureServer is a stateful MCP server with tools, resources and prompts.
func featureServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			return
		}
		b, _ := io.ReadAll(r.Body)
		var m struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.Unmarshal(b, &m)
		var result any
		switch m.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"resources": map[string]any{}, "prompts": map[string]any{}}}
		case "notifications/initialized":
			w.WriteHeader(202)
			return
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}
		case "resources/list":
			result = map[string]any{"resources": []any{map[string]any{"uri": "demo://docs/readme", "name": "readme"}}}
		case "resources/templates/list":
			result = map[string]any{"resourceTemplates": []any{map[string]any{"uriTemplate": "demo://items/{id}", "name": "item"}}}
		case "resources/read":
			uri, _ := m.Params["uri"].(string)
			if uri == "demo://missing" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + mustJSON(m.ID) + `,"error":{"code":-32002,"message":"Resource not found"}}`))
				return
			}
			result = map[string]any{"contents": []any{map[string]any{"uri": uri, "text": "body of " + uri}}}
		case "prompts/list":
			result = map[string]any{"prompts": []any{map[string]any{"name": "summarize", "arguments": []any{map[string]any{"name": "topic", "required": true}}}}}
		case "prompts/get":
			args, _ := m.Params["arguments"].(map[string]any)
			result = map[string]any{"description": "d", "messages": []any{map[string]any{"role": "user",
				"content": map[string]any{"type": "text", "text": "Summarize " + args["topic"].(string)}}}}
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestJSResourcesAndPrompts(t *testing.T) {
	srv := featureServer(t)
	rt := modulestest.NewRuntime(t)
	registry := rt.VU.InitEnvField.Registry
	m := New().NewModuleInstance(rt.VU).(*ModuleInstance)
	if err := rt.VU.Runtime().Set("mcp", m.Exports().Named); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RunOnEventLoop(`var client = new mcp.Client({url: "` + srv.URL + `", protocol: "2025-06-18", timeout: "5s"});`); err != nil {
		t.Fatal(err)
	}
	samples := make(chan metrics.SampleContainer, 1000)
	rt.MoveToVUContext(&lib.State{
		Samples:   samples,
		Tags:      lib.NewVUStateTags(registry.RootTagSet()),
		Transport: http.DefaultTransport,
	})
	v, err := rt.RunOnEventLoop(`
		const s = client.connect();
		const rs = s.listResources();
		const ts = s.listResourceTemplates();
		const ps = s.listPrompts();
		const r1 = s.readResource("demo://docs/readme");
		const r2 = s.readResource("demo://items/7", {meta: {k: 1}, cancelAfterMs: 5000});
		const miss = s.readResource("demo://missing");
		const p = s.getPrompt("summarize", {topic: 42});
		const par = s.callParallel([
			{name: "echo"},
			{kind: "resource", uri: "demo://items/8"},
			{kind: "prompt", name: "summarize", args: {topic: "x"}},
		]);
		let badKind; try { s.callParallel([{kind: "nope", name: "x"}]); } catch (e) { badKind = String(e).includes("kind must be"); }
		let badOpt; try { s.readResource("demo://x", {bogus: 1}); } catch (e) { badOpt = String(e).includes("unknown option"); }
		s.close();
		JSON.stringify({
			rs: rs.map(x => x.name), ts: ts[0].uriTemplate, ps: ps[0].arguments[0].required,
			r1: [r1.isError, r1.resource, r1.contents[0].text, typeof r1.durationMs],
			r2: r2.resource,
			miss: [miss.isError, miss.error.code],
			p: [p.description, p.messages[0].content.text],
			par: [par[0].content[0].text, par[1].resource, par[2].messages[0].content.text],
			badKind, badOpt,
		});
	`)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"rs":["readme"],"ts":"demo://items/{id}","ps":true,"r1":[false,"readme","body of demo://docs/readme","number"],"r2":"item","miss":[true,-32002],"p":["d","Summarize 42"],"par":["ok","item","Summarize x"],"badKind":true,"badOpt":true}`
	if v.String() != want {
		t.Fatalf("got  %s\nwant %s", v, want)
	}

	close(samples)
	resTags, promptTags := map[string]int{}, map[string]int{}
	toolRate := 0
	for sc := range samples {
		for _, s := range sc.GetSamples() {
			if s.Metric.Name == "mcp_tool_error_rate" {
				toolRate++
			}
			if s.Metric.Name != "mcp_req_duration" {
				continue
			}
			method, _ := s.Tags.Get("method")
			res, hasRes := s.Tags.Get("resource")
			pr, hasPrompt := s.Tags.Get("prompt")
			if _, hasTool := s.Tags.Get("tool"); hasTool && method != "tools/call" {
				t.Errorf("tool tag on %s", method)
			}
			switch method {
			case "resources/read":
				resTags[res]++
			case "prompts/get":
				promptTags[pr]++
			default:
				if hasRes || hasPrompt {
					t.Errorf("resource/prompt tag on %s: %v", method, s.Tags.Map())
				}
			}
		}
	}
	// demo://missing has no listed name: its fallback label is scheme+host.
	if resTags["readme"] != 1 || resTags["item"] != 2 || resTags["demo://missing"] != 1 || len(resTags) != 3 {
		t.Errorf("resource tags: %v", resTags)
	}
	if promptTags["summarize"] != 2 || len(promptTags) != 1 {
		t.Errorf("prompt tags: %v", promptTags)
	}
	if toolRate != 1 {
		t.Errorf("mcp_tool_error_rate must only count tools/call, got %d samples", toolRate)
	}
}
