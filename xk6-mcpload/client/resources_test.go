package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// handleFeatures answers the resources and prompts methods of the fake
// server (called from fakeServer.handle). It reports whether it handled
// method.
func (f *fakeServer) handleFeatures(w http.ResponseWriter, id any, method string, params map[string]any) bool {
	switch method {
	case "resources/list":
		if cursor, _ := params["cursor"].(string); cursor == "" {
			f.reply(w, id, map[string]any{
				"resources":  []any{map[string]any{"uri": "demo://docs/readme", "name": "readme", "mimeType": "text/plain"}},
				"nextCursor": "r2",
			})
			return true
		}
		f.reply(w, id, map[string]any{"resources": []any{map[string]any{"uri": "demo://docs/changelog", "name": "changelog"}}})
	case "resources/templates/list":
		f.reply(w, id, map[string]any{"resourceTemplates": []any{
			map[string]any{"uriTemplate": "demo://items/{id}", "name": "item"},
		}})
	case "resources/read":
		uri, _ := params["uri"].(string)
		if uri == "demo://missing" {
			writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{
				"code": -32002, "message": "Resource not found", "data": map[string]any{"uri": uri}}})
			return true
		}
		f.reply(w, id, map[string]any{"contents": []any{
			map[string]any{"uri": uri, "mimeType": "text/plain", "text": "content of " + uri},
		}})
	case "prompts/list":
		f.reply(w, id, map[string]any{"prompts": []any{map[string]any{
			"name": "summarize", "description": "summarizes a topic",
			"arguments": []any{map[string]any{"name": "topic", "required": true}},
		}}})
	case "prompts/get":
		name, _ := params["name"].(string)
		if name != "summarize" {
			writeJSON(w, 200, rpcErr(id, -32602, "unknown prompt"))
			return true
		}
		args, _ := params["arguments"].(map[string]any)
		f.reply(w, id, map[string]any{
			"description": "summary prompt",
			"messages": []any{map[string]any{"role": "user", "content": map[string]any{
				"type": "text", "text": fmt.Sprint("Summarize ", args["topic"])}}},
		})
	default:
		return false
	}
	return true
}

func TestResourcesAndPrompts(t *testing.T) {
	for _, mode := range []struct {
		name     string
		protocol string
		sse      bool
	}{
		{"stateful-json", "2025-06-18", false},
		{"stateful-sse", "2025-06-18", true},
		{"stateless", ProtocolStateless, false},
	} {
		t.Run(mode.name, func(t *testing.T) {
			f, srv := newFake(t, IsStateless(mode.protocol))
			f.sse = mode.sse
			obs := &collector{}
			s := connect(t, srv.URL, mode.protocol, obs)
			ctx := context.Background()

			rs, err := s.ListResources(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(rs) != 2 || rs[0].Name != "readme" || rs[0].MimeType != "text/plain" || rs[1].URI != "demo://docs/changelog" {
				t.Fatalf("resources: %+v", rs)
			}
			if n := len(obs.byMethod("resources/list")); n != 2 {
				t.Fatalf("expected 2 resources/list pages, got %d", n)
			}
			ts, err := s.ListResourceTemplates(ctx)
			if err != nil || len(ts) != 1 || ts[0].Name != "item" {
				t.Fatalf("templates: %+v %v", ts, err)
			}
			ps, err := s.ListPrompts(ctx)
			if err != nil || len(ps) != 1 || ps[0].Name != "summarize" || len(ps[0].Arguments) != 1 || !ps[0].Arguments[0].Required {
				t.Fatalf("prompts: %+v %v", ps, err)
			}

			for _, c := range []struct{ uri, label string }{
				{"demo://docs/readme", "readme"},       // listed by name
				{"demo://items/42", "item"},            // matches a template
				{"other://host/a/b", "other://host/a"}, // fallback label
			} {
				r := s.ReadResource(ctx, c.uri)
				if r.Err != nil || r.IsError || r.Label != c.label || !strings.Contains(string(r.Contents), "content of "+c.uri) {
					t.Fatalf("read %s: %+v %v", c.uri, r, r.Err)
				}
				if mode.protocol == ProtocolStateless {
					f.mu.Lock()
					hdr := f.lastHdr.Get(HeaderName)
					f.mu.Unlock()
					if hdr != c.uri {
						t.Errorf("Mcp-Name for %s: %q", c.uri, hdr)
					}
				}
			}
			reads := obs.byMethod("resources/read")
			if len(reads) != 3 || reads[0].Resource != "readme" || reads[1].Resource != "item" || reads[2].Resource != "other://host/a" {
				t.Fatalf("read stats: %+v", reads)
			}
			for _, st := range reads {
				if st.Tool != "" || st.Prompt != "" || st.Status != 200 || st.ErrorType != "" {
					t.Errorf("read stat: %+v", st)
				}
			}

			miss := s.ReadResource(ctx, "demo://missing")
			if !miss.IsError || miss.Err == nil || miss.Err.Code != -32002 || miss.Err.Type != ErrJSONRPC || miss.Cancelled {
				t.Fatalf("missing resource: %+v %+v", miss, miss.Err)
			}

			p := s.GetPrompt(ctx, "summarize", map[string]string{"topic": "load"})
			if p.Err != nil || p.Description != "summary prompt" || !strings.Contains(string(p.Messages), "Summarize load") {
				t.Fatalf("prompt: %+v %v", p, p.Err)
			}
			f.mu.Lock()
			args, _ := f.lastBody["arguments"].(map[string]any)
			f.mu.Unlock()
			if args["topic"] != "load" {
				t.Errorf("prompt arguments sent: %v", args)
			}
			bad := s.GetPrompt(ctx, "nope", nil)
			if !bad.IsError || bad.Err == nil || bad.Err.Code != -32602 {
				t.Fatalf("unknown prompt: %+v %+v", bad, bad.Err)
			}
			gets := obs.byMethod("prompts/get")
			if len(gets) != 2 || gets[0].Prompt != "summarize" || gets[0].Resource != "" || gets[1].ErrorType == "" {
				t.Fatalf("prompt stats: %+v", gets)
			}
		})
	}
}

func TestResourceNameHeaderEncoded(t *testing.T) {
	f, srv := newFake(t, true)
	s := connect(t, srv.URL, ProtocolStateless, nil)
	uri := "demo://docs/résumé"
	r := s.ReadResource(context.Background(), uri)
	if r.Err != nil {
		t.Fatalf("read: %v", r.Err)
	}
	f.mu.Lock()
	hdr := f.lastHdr.Get(HeaderName)
	f.mu.Unlock()
	if !strings.HasPrefix(hdr, "=?base64?") {
		t.Fatalf("non-ASCII uri must be base64-wrapped in Mcp-Name, got %q", hdr)
	}
}

func TestParallelMixed(t *testing.T) {
	_, srv := newFake(t, false)
	obs := &collector{}
	s := connect(t, srv.URL, "2025-06-18", obs)
	out := s.Parallel(context.Background(), []Call{
		{Name: "echo", Args: map[string]any{"msg": "a"}},
		{Kind: KindResource, URI: "demo://items/7"},
		{Kind: KindPrompt, Name: "summarize", PromptArgs: map[string]string{"topic": "x"}},
	})
	if len(out) != 3 {
		t.Fatalf("got %d results", len(out))
	}
	if out[0].Kind != KindTool || out[0].Tool.Err != nil || !strings.Contains(string(out[0].Tool.Content), `"a"`) {
		t.Errorf("tool: %+v", out[0])
	}
	if out[1].Kind != KindResource || out[1].Resource.Err != nil || !strings.Contains(string(out[1].Resource.Contents), "demo://items/7") {
		t.Errorf("resource: %+v", out[1])
	}
	if out[2].Kind != KindPrompt || out[2].Prompt.Err != nil || !strings.Contains(string(out[2].Prompt.Messages), "Summarize x") {
		t.Errorf("prompt: %+v", out[2])
	}
	if c := obs.byMethod("tools/call"); len(c) != 1 || c[0].Tool != "echo" || c[0].Resource != "" {
		t.Errorf("tool stats: %+v", c)
	}
}

func TestTemplateRegexp(t *testing.T) {
	for _, c := range []struct {
		tpl   string
		uri   string
		match bool
	}{
		{"demo://items/{id}", "demo://items/42", true},
		{"demo://items/{id}", "demo://items/42/sub", false},
		{"demo://items/{id}", "demo://other/42", false},
		{"file:///{+path}", "file:///a/b/c.txt", true},
		{"db://users/{id}{?fields}", "db://users/9?fields=name", true},
		{"a.b://{x}", "aXb://1", false}, // literal parts are quoted
	} {
		re := templateRegexp(c.tpl)
		if re == nil {
			t.Fatalf("%s: nil regexp", c.tpl)
		}
		if got := re.MatchString(c.uri); got != c.match {
			t.Errorf("%s ~ %s = %v, want %v", c.tpl, c.uri, got, c.match)
		}
	}
	if templateRegexp("demo://{broken") != nil || templateRegexp("") != nil {
		t.Error("malformed templates must give nil")
	}
}

func TestResourceFallbackLabel(t *testing.T) {
	for in, want := range map[string]string{
		"file:///projects/app/a.json":            "file:///projects",
		"demo://items/42":                        "demo://items",
		"https://x.test/readme":                  "https://x.test",
		"https://user:secret@x.test/a/b?tok=1#f": "https://x.test/a",
		"urn:isbn:0451450523":                    "urn:",
		"no scheme":                              OtherLabel,
		"%zz":                                    OtherLabel,
	} {
		if got := resourceFallbackLabel(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestLabelSetBounded(t *testing.T) {
	l := &labelSet{max: 2}
	if l.get("a") != "a" || l.get("b") != "b" || l.get("c") != OtherLabel || l.get("a") != "a" || l.get("") != OtherLabel {
		t.Fatal("labelSet must keep the first max values and map the rest to OtherLabel")
	}
}

func TestResourceResultJSONRoundTrip(t *testing.T) {
	// Contents is passed through untouched (text and blob entries alike).
	_, srv := newFake(t, false)
	s := connect(t, srv.URL, "2025-06-18", nil)
	r := s.ReadResource(context.Background(), "demo://docs/readme")
	var items []map[string]any
	if err := json.Unmarshal(r.Contents, &items); err != nil || len(items) != 1 || items[0]["mimeType"] != "text/plain" {
		t.Fatalf("contents: %s %v", r.Contents, err)
	}
}
