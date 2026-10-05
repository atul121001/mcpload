// Package mcpload is a k6 extension (JS import "k6/x/mcpload") for load
// testing remote MCP servers over streamable HTTP.
package mcpload

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/grafana/sobek"
	"go.k6.io/k6/v2/js/common"
	"go.k6.io/k6/v2/js/modules"

	"github.com/atul121001/mcpload/xk6-mcpload/client"
)

const importPath = "k6/x/mcpload"

func init() {
	modules.Register(importPath, New())
}

type (
	// RootModule is the global module object.
	RootModule struct{}

	// ModuleInstance is created once per VU.
	ModuleInstance struct {
		vu      modules.VU
		metrics *mcpMetrics
		hc      *http.Client // per-VU, built lazily from vu.State().Transport
	}
)

var (
	_ modules.Module   = &RootModule{}
	_ modules.Instance = &ModuleInstance{}
)

// New returns the root module.
func New() *RootModule { return &RootModule{} }

// NewModuleInstance implements modules.Module.
func (*RootModule) NewModuleInstance(vu modules.VU) modules.Instance {
	m, err := registerMetrics(vu.InitEnv().Registry)
	if err != nil {
		common.Throw(vu.Runtime(), fmt.Errorf("mcpload: registering metrics: %w", err))
	}
	return &ModuleInstance{vu: vu, metrics: m}
}

// Exports implements modules.Instance. `import mcp from 'k6/x/mcpload'`
// gets an object built from the named exports.
func (mi *ModuleInstance) Exports() modules.Exports {
	return modules.Exports{Named: map[string]any{
		"Client":  mi.newClient,
		"version": client.Version,
	}}
}

// jsClient holds the configuration given to `new mcp.Client(...)`.
type jsClient struct {
	mi              *ModuleInstance
	opts            client.Options
	includePayloads bool
	// rememberProtocol: with protocol "auto", reuse the protocol resolved by
	// the first successful connect (process-wide, across VUs, keyed by URL
	// and protocol options) instead of probing on every connect.
	rememberProtocol bool
}

func (mi *ModuleInstance) newClient(call sobek.ConstructorCall) *sobek.Object {
	rt := mi.vu.Runtime()
	c := &jsClient{mi: mi, rememberProtocol: true}
	if err := c.parseOptions(rt, call.Argument(0)); err != nil {
		common.Throw(rt, fmt.Errorf("mcp.Client: %w", err))
	}
	obj := rt.NewObject()
	must(rt, obj.Set("connect", c.connect))
	must(rt, obj.Set("url", c.opts.URL))
	must(rt, obj.Set("protocol", c.opts.Protocol))
	return obj
}

func must(rt *sobek.Runtime, err error) {
	if err != nil {
		common.Throw(rt, err)
	}
}

func (c *jsClient) parseOptions(rt *sobek.Runtime, v sobek.Value) error {
	if common.IsNullish(v) {
		return errors.New("options object with a url is required")
	}
	raw, ok := v.Export().(map[string]any)
	if !ok {
		return errors.New("options must be an object")
	}
	o := client.Options{Protocol: client.ProtocolAuto, Timeout: 30 * time.Second}
	for k, val := range raw {
		switch k {
		case "url":
			o.URL = fmt.Sprint(val)
		case "protocol":
			o.Protocol = fmt.Sprint(val)
		case "fallbackVersion":
			o.FallbackVersion = fmt.Sprint(val)
		case "headers":
			hm, ok := val.(map[string]any)
			if !ok {
				return errors.New("headers must be an object")
			}
			o.Headers = make(map[string]string, len(hm))
			for hk, hv := range hm {
				o.Headers[hk] = fmt.Sprint(hv)
			}
		case "timeout":
			d, err := parseDuration(val)
			if err != nil {
				return fmt.Errorf("timeout: %w", err)
			}
			o.Timeout = d
		case "cancelWait":
			d, err := parseDuration(val)
			if err != nil {
				return fmt.Errorf("cancelWait: %w", err)
			}
			if d < 0 {
				return errors.New("cancelWait must not be negative")
			}
			o.CancelWait = d
		case "includePayloads":
			b, _ := val.(bool)
			c.includePayloads = b
		case "servedByHeader":
			o.ServedByHeader = fmt.Sprint(val)
		case "rememberProtocol":
			if b, ok := val.(bool); ok {
				c.rememberProtocol = b
			}
		case "discover":
			if b, ok := val.(bool); ok {
				o.SkipDiscover = !b
			}
		case "clientInfo":
			if m, ok := val.(map[string]any); ok {
				o.ClientInfo = client.Implementation{Name: str(m["name"]), Version: str(m["version"])}
			}
		case "capabilities":
			if m, ok := val.(map[string]any); ok {
				o.Capabilities = m
			}
		case "sampling", "elicitation", "roots":
			r, err := parseResponder(k, val)
			if err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
			switch k {
			case "sampling":
				o.Sampling = r
			case "elicitation":
				o.Elicitation = r
			default:
				o.Roots = r
			}
		case "auth":
			a, err := parseAuth(val)
			if err != nil {
				return fmt.Errorf("auth: %w", err)
			}
			o.Auth = a
		default:
			return fmt.Errorf("unknown option %q", k)
		}
	}
	if err := client.ValidateProtocol(o.Protocol); err != nil {
		return err
	}
	if err := client.ValidateFallbackVersion(o.FallbackVersion); err != nil {
		return err
	}
	if o.URL == "" {
		return errors.New("url is required")
	}
	c.opts = o
	return nil
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func parseDuration(v any) (time.Duration, error) {
	switch t := v.(type) {
	case string:
		return time.ParseDuration(t)
	case int64:
		return time.Duration(t) * time.Millisecond, nil
	case float64:
		return time.Duration(t * float64(time.Millisecond)), nil
	}
	return 0, fmt.Errorf("expected a duration string like '30s' or milliseconds, got %T", v)
}

func parseAuth(v any) (client.Auth, error) {
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("must be an object")
	}
	switch strings.ToLower(str(m["type"])) {
	case "bearer":
		tok := str(m["token"])
		if tok == "" {
			return nil, errors.New("bearer auth needs a token")
		}
		return client.BearerAuth{Token: tok}, nil
	case "oauth", "client_credentials":
		cfg := client.OAuthConfig{
			TokenURL:     str(m["tokenUrl"]),
			ClientID:     str(m["clientId"]),
			ClientSecret: str(m["clientSecret"]),
			Scope:        str(m["scope"]),
			Audience:     str(m["audience"]),
			AuthStyle:    str(m["authStyle"]),
		}
		if fb, ok := m["failureBackoff"]; ok && fb != nil {
			d, err := parseDuration(fb)
			if err != nil {
				return nil, fmt.Errorf("failureBackoff: %w", err)
			}
			if d <= 0 {
				d = -1 // explicit 0 disables the negative cache
			}
			cfg.FailureBackoff = d
		}
		if to, ok := m["timeout"]; ok && to != nil {
			d, err := parseDuration(to)
			if err != nil {
				return nil, fmt.Errorf("timeout: %w", err)
			}
			if d < 0 {
				return nil, errors.New("timeout must not be negative")
			}
			cfg.Timeout = d // 0: use the client's timeout option
		}
		if cfg.TokenURL == "" || cfg.ClientID == "" {
			return nil, errors.New("oauth auth needs tokenUrl and clientId")
		}
		// Shared across VUs: one cached token per credential set, single-flight refresh.
		return client.SharedTokenSource(cfg), nil
	case "", "none":
		return nil, nil
	}
	return nil, fmt.Errorf("unsupported auth type %q", m["type"])
}

// Default answers for `sampling: true` / `elicitation: true` / `roots: true`.
var defaultResponses = map[string]map[string]any{
	"sampling": {
		"role":       "assistant",
		"content":    map[string]any{"type": "text", "text": "mcpload mock response"},
		"model":      "mcpload-mock",
		"stopReason": "endTurn",
	},
	"elicitation": {"action": "accept", "content": map[string]any{}},
	"roots":       {"roots": []any{}},
}

// parseResponder turns a sampling / elicitation / roots option into a static
// client.Responder. Answers are fixed when the Client is constructed: they
// are sent from Go goroutines while a call (or callParallel) is in flight,
// where the VU's JS runtime must not be touched, so JS callbacks are not
// supported.
//
//	true | {}                                    default answer
//	{ delayMs, error: {code, message} }          answer with a JSON-RPC error
//	sampling:    { response: {role, content, model, stopReason}, delayMs }
//	elicitation: { action: 'accept'|'decline'|'cancel', content: {...}, delayMs }
//	roots:       { roots: [{uri, name}], delayMs }
func parseResponder(kind string, v any) (*client.Responder, error) {
	var m map[string]any
	switch t := v.(type) {
	case nil:
		return nil, nil
	case bool:
		if !t {
			return nil, nil
		}
		m = map[string]any{}
	case map[string]any:
		m = t
	default:
		return nil, errors.New("must be true or an object")
	}
	r := &client.Responder{}
	result := map[string]any{}
	for k, val := range defaultResponses[kind] {
		result[k] = val
	}
	for k, val := range m {
		switch {
		case k == "delayMs":
			d, err := parseDuration(val)
			if err != nil {
				return nil, fmt.Errorf("delayMs: %w", err)
			}
			if d < 0 {
				return nil, errors.New("delayMs must not be negative")
			}
			r.Delay = d
		case k == "error":
			em, ok := val.(map[string]any)
			if !ok {
				return nil, errors.New("error must be {code, message}")
			}
			code, ok := toInt(em["code"])
			if !ok {
				return nil, errors.New("error.code must be an integer")
			}
			r.Error = &client.ResponderError{Code: code, Message: str(em["message"])}
		case kind == "sampling" && k == "response":
			rm, ok := val.(map[string]any)
			if !ok {
				return nil, errors.New("response must be an object")
			}
			result = rm
		case kind == "elicitation" && k == "action":
			a := str(val)
			if a != "accept" && a != "decline" && a != "cancel" {
				return nil, fmt.Errorf("action must be 'accept', 'decline' or 'cancel', got %q", a)
			}
			result["action"] = a
		case kind == "elicitation" && k == "content":
			if _, ok := val.(map[string]any); !ok {
				return nil, errors.New("content must be an object")
			}
			result["content"] = val
		case kind == "roots" && k == "roots":
			if _, ok := val.([]any); !ok {
				return nil, errors.New("roots must be an array of {uri, name}")
			}
			result["roots"] = val
		default:
			return nil, fmt.Errorf("unknown option %q", k)
		}
	}
	if kind == "elicitation" && result["action"] != "accept" {
		delete(result, "content") // content is only sent with accept
	}
	b, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	r.Result = b
	return r, nil
}

func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case int64:
		return int(t), true
	case float64:
		if t == float64(int(t)) {
			return int(t), true
		}
	}
	return 0, false
}

// emitter captures the VU's current tags on the JS thread.
func (mi *ModuleInstance) emitter() *emitter {
	state := mi.vu.State()
	tm := state.Tags.GetCurrentValues()
	return &emitter{
		ctx:        mi.vu.Context(),
		samples:    state.Samples,
		m:          mi.metrics,
		tags:       tm.Tags,
		meta:       tm.Metadata,
		stableTags: mi.metrics.root.WithTagsFromMap(state.Options.RunTags),
	}
}

func (mi *ModuleInstance) httpClient() *http.Client {
	if mi.hc == nil {
		state := mi.vu.State()
		// The VU's transport carries k6's dialer (data_sent/received metrics,
		// DNS and blacklist options) and TLS configuration.
		mi.hc = &http.Client{Transport: state.Transport}
	}
	return mi.hc
}

func (c *jsClient) connect() *sobek.Object {
	mi := c.mi
	rt := mi.vu.Runtime()
	if mi.vu.State() == nil {
		common.Throw(rt, errors.New("mcp: connect() must be called in the VU context (default function, setup or teardown), not in the init context"))
	}
	opts := c.opts
	if c.rememberProtocol {
		opts.ProtocolCache = client.SharedProtocolCache
	}
	opts.HTTPClient = mi.httpClient()
	opts.Observer = mi.emitter()
	s, err := client.Connect(mi.vu.Context(), opts)
	if err != nil {
		throwMCP(rt, client.AsError(err))
	}
	return newJSSession(mi, s, c.includePayloads)
}

type jsSession struct {
	mi              *ModuleInstance
	s               *client.Session
	includePayloads bool
}

func newJSSession(mi *ModuleInstance, s *client.Session, includePayloads bool) *sobek.Object {
	rt := mi.vu.Runtime()
	js := &jsSession{mi: mi, s: s, includePayloads: includePayloads}
	obj := rt.NewObject()
	must(rt, obj.Set("protocol", s.Protocol()))
	must(rt, obj.Set("sessionId", s.SessionID()))
	must(rt, obj.Set("stateless", s.Stateless()))
	must(rt, obj.Set("servedBy", s.ServedBy()))
	must(rt, obj.Set("listTools", js.listTools))
	must(rt, obj.Set("callTool", js.callTool))
	must(rt, obj.Set("callParallel", js.callParallel))
	must(rt, obj.Set("listResources", js.listResources))
	must(rt, obj.Set("listResourceTemplates", js.listResourceTemplates))
	must(rt, obj.Set("listPrompts", js.listPrompts))
	must(rt, obj.Set("readResource", js.readResource))
	must(rt, obj.Set("getPrompt", js.getPrompt))
	must(rt, obj.Set("ping", js.ping))
	must(rt, obj.Set("close", js.close))
	return obj
}

func (js *jsSession) ctx() (ctxObs client.Observer) { return js.mi.emitter() }

func (js *jsSession) listTools() sobek.Value {
	rt := js.mi.vu.Runtime()
	ctx := client.WithObserver(js.mi.vu.Context(), js.ctx())
	tools, err := js.s.ListTools(ctx)
	if err != nil {
		throwMCP(rt, client.AsError(err))
	}
	out := make([]map[string]any, len(tools))
	for i, t := range tools {
		out[i] = map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema}
	}
	return toJS(rt, out)
}

// callTool(name, args?, opts?): opts.meta is sent as params._meta;
// opts.cancelAfterMs cancels the call when no response arrived within it.
func (js *jsSession) callTool(name string, args sobek.Value, opts sobek.Value) sobek.Value {
	rt := js.mi.vu.Runtime()
	a := exportArgs(args)
	var meta map[string]any
	var co client.CallOptions
	if !common.IsNullish(opts) {
		m, ok := opts.Export().(map[string]any)
		if !ok {
			common.Throw(rt, errors.New("callTool: options must be an object like {meta, cancelAfterMs}"))
		}
		var err error
		if err = checkCallKeys(m, "options"); err != nil {
			common.Throw(rt, fmt.Errorf("callTool: %w", err))
		}
		if meta, err = exportMeta(m["meta"]); err != nil {
			common.Throw(rt, fmt.Errorf("callTool: %w", err))
		}
		if co, err = parseCallOptions(m, "options"); err != nil {
			common.Throw(rt, fmt.Errorf("callTool: %w", err))
		}
	}
	ctx := client.WithObserver(js.mi.vu.Context(), js.ctx())
	r := js.s.CallToolMetaWith(ctx, name, a, meta, co)
	return toJS(rt, toolResultJSON(r, js.includePayloads))
}

// exportMeta checks a JS meta value: absent or an object.
func exportMeta(v any) (map[string]any, error) {
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("meta must be an object")
	}
	return m, nil
}

// callKeys are the per-call option keys of callTool's third argument and of
// callParallel items (which also hold name and args).
var callKeys = map[string]bool{"meta": true, "cancelAfterMs": true}

// checkCallKeys rejects keys of per-call options that are not callKeys (or
// one of extra), like unknown client options are rejected.
func checkCallKeys(m map[string]any, what string, extra ...string) error {
	for k := range m {
		if callKeys[k] || slices.Contains(extra, k) {
			continue
		}
		return fmt.Errorf("%s: unknown option %q", what, k)
	}
	return nil
}

// parseCallOptions reads the per-call options of callTool's third argument
// and of callParallel items: cancelAfterMs (milliseconds or a duration
// string; 0 or absent = never cancel). Keys of m it does not know are left
// to the caller (meta is read by exportMeta; callParallel items also hold
// name and args).
func parseCallOptions(m map[string]any, what string) (client.CallOptions, error) {
	var co client.CallOptions
	if v, ok := m["cancelAfterMs"]; ok && v != nil {
		d, err := parseDuration(v)
		if err != nil {
			return co, fmt.Errorf("%s.cancelAfterMs: %w", what, err)
		}
		if d < 0 {
			return co, fmt.Errorf("%s.cancelAfterMs must not be negative", what)
		}
		co.CancelAfter = d
	}
	return co, nil
}

// callParallel runs a batch concurrently. Each element is a tool call
// {name, args} (kind absent or "tool"), a resource read {kind: "resource",
// uri} or a prompt get {kind: "prompt", name, args}; all accept meta and
// cancelAfterMs. Results come back in input order, each shaped like the
// result of callTool / readResource / getPrompt.
func (js *jsSession) callParallel(v sobek.Value) sobek.Value {
	rt := js.mi.vu.Runtime()
	if common.IsNullish(v) {
		common.Throw(rt, errors.New("callParallel expects an array of {name, args}"))
	}
	list, ok := v.Export().([]any)
	if !ok {
		common.Throw(rt, errors.New("callParallel expects an array of {name, args}"))
	}
	// Everything is extracted from JS values here, on the JS thread. The
	// goroutines below never touch the runtime.
	calls := make([]client.Call, len(list))
	for i, item := range list {
		what := fmt.Sprintf("element %d", i)
		m, ok := item.(map[string]any)
		if !ok {
			common.Throw(rt, fmt.Errorf("callParallel: %s must be an object", what))
		}
		var c client.Call
		switch kind := str(m["kind"]); kind {
		case "", "tool":
			if str(m["name"]) == "" {
				common.Throw(rt, fmt.Errorf("callParallel: %s must be {name, args}", what))
			}
			c = client.Call{Kind: client.KindTool, Name: str(m["name"]), Args: m["args"]}
		case client.KindResource:
			if str(m["uri"]) == "" {
				common.Throw(rt, fmt.Errorf("callParallel: %s must be {kind: 'resource', uri}", what))
			}
			c = client.Call{Kind: client.KindResource, URI: str(m["uri"])}
		case client.KindPrompt:
			if str(m["name"]) == "" {
				common.Throw(rt, fmt.Errorf("callParallel: %s must be {kind: 'prompt', name, args}", what))
			}
			pa, err := exportPromptArgs(m["args"])
			if err != nil {
				common.Throw(rt, fmt.Errorf("callParallel: %s: %w", what, err))
			}
			c = client.Call{Kind: client.KindPrompt, Name: str(m["name"]), PromptArgs: pa}
		default:
			common.Throw(rt, fmt.Errorf("callParallel: %s: kind must be 'tool', 'resource' or 'prompt', got %q", what, kind))
		}
		if err := checkCallKeys(m, what, "kind", "name", "args", "uri"); err != nil {
			common.Throw(rt, fmt.Errorf("callParallel: %w", err))
		}
		meta, err := exportMeta(m["meta"])
		if err != nil {
			common.Throw(rt, fmt.Errorf("callParallel: %s: %w", what, err))
		}
		co, err := parseCallOptions(m, what)
		if err != nil {
			common.Throw(rt, fmt.Errorf("callParallel: %w", err))
		}
		c.Meta, c.CallOptions = meta, co
		calls[i] = c
	}
	ctx := client.WithObserver(js.mi.vu.Context(), js.ctx())
	results := js.s.Parallel(ctx, calls) // returns after all goroutines finish
	out := make([]map[string]any, len(results))
	for i, r := range results {
		switch r.Kind {
		case client.KindResource:
			out[i] = resourceResultJSON(r.Resource)
		case client.KindPrompt:
			out[i] = promptResultJSON(r.Prompt)
		default:
			out[i] = toolResultJSON(r.Tool, js.includePayloads)
		}
	}
	return toJS(rt, out)
}

func (js *jsSession) listResources() sobek.Value {
	rt := js.mi.vu.Runtime()
	ctx := client.WithObserver(js.mi.vu.Context(), js.ctx())
	rs, err := js.s.ListResources(ctx)
	if err != nil {
		throwMCP(rt, client.AsError(err))
	}
	if rs == nil {
		rs = []client.Resource{}
	}
	return toJS(rt, rs)
}

func (js *jsSession) listResourceTemplates() sobek.Value {
	rt := js.mi.vu.Runtime()
	ctx := client.WithObserver(js.mi.vu.Context(), js.ctx())
	ts, err := js.s.ListResourceTemplates(ctx)
	if err != nil {
		throwMCP(rt, client.AsError(err))
	}
	if ts == nil {
		ts = []client.ResourceTemplate{}
	}
	return toJS(rt, ts)
}

func (js *jsSession) listPrompts() sobek.Value {
	rt := js.mi.vu.Runtime()
	ctx := client.WithObserver(js.mi.vu.Context(), js.ctx())
	ps, err := js.s.ListPrompts(ctx)
	if err != nil {
		throwMCP(rt, client.AsError(err))
	}
	if ps == nil {
		ps = []client.Prompt{}
	}
	return toJS(rt, ps)
}

// callOpts reads the {meta, cancelAfterMs} options object of readResource
// and getPrompt (callTool reads the same keys).
func callOpts(rt *sobek.Runtime, fn string, opts sobek.Value) (map[string]any, client.CallOptions) {
	if common.IsNullish(opts) {
		return nil, client.CallOptions{}
	}
	m, ok := opts.Export().(map[string]any)
	if !ok {
		common.Throw(rt, fmt.Errorf("%s: options must be an object like {meta, cancelAfterMs}", fn))
	}
	if err := checkCallKeys(m, "options"); err != nil {
		common.Throw(rt, fmt.Errorf("%s: %w", fn, err))
	}
	meta, err := exportMeta(m["meta"])
	if err != nil {
		common.Throw(rt, fmt.Errorf("%s: %w", fn, err))
	}
	co, err := parseCallOptions(m, "options")
	if err != nil {
		common.Throw(rt, fmt.Errorf("%s: %w", fn, err))
	}
	return meta, co
}

// readResource(uri, opts?): opts as for callTool.
func (js *jsSession) readResource(uri string, opts sobek.Value) sobek.Value {
	rt := js.mi.vu.Runtime()
	if uri == "" {
		common.Throw(rt, errors.New("readResource: uri is required"))
	}
	meta, co := callOpts(rt, "readResource", opts)
	ctx := client.WithObserver(js.mi.vu.Context(), js.ctx())
	return toJS(rt, resourceResultJSON(js.s.ReadResourceWith(ctx, uri, meta, co)))
}

// getPrompt(name, args?, opts?): args values are sent as strings; opts as
// for callTool.
func (js *jsSession) getPrompt(name string, args sobek.Value, opts sobek.Value) sobek.Value {
	rt := js.mi.vu.Runtime()
	if name == "" {
		common.Throw(rt, errors.New("getPrompt: name is required"))
	}
	var pa map[string]string
	if !common.IsNullish(args) {
		var err error
		if pa, err = exportPromptArgs(args.Export()); err != nil {
			common.Throw(rt, fmt.Errorf("getPrompt: %w", err))
		}
	}
	meta, co := callOpts(rt, "getPrompt", opts)
	ctx := client.WithObserver(js.mi.vu.Context(), js.ctx())
	return toJS(rt, promptResultJSON(js.s.GetPromptWith(ctx, name, pa, meta, co)))
}

// exportPromptArgs checks prompt arguments: absent or an object. MCP prompt
// arguments are strings, so other values are converted with fmt.Sprint.
func exportPromptArgs(v any) (map[string]string, error) {
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("prompt args must be an object")
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		out[k] = str(val)
	}
	return out, nil
}

func (js *jsSession) ping() {
	ctx := client.WithObserver(js.mi.vu.Context(), js.ctx())
	if err := js.s.Ping(ctx); err != nil {
		throwMCP(js.mi.vu.Runtime(), client.AsError(err))
	}
}

func (js *jsSession) close() {
	ctx := client.WithObserver(js.mi.vu.Context(), js.ctx())
	if err := js.s.Close(ctx); err != nil {
		throwMCP(js.mi.vu.Runtime(), client.AsError(err))
	}
}

func exportArgs(v sobek.Value) any {
	if common.IsNullish(v) {
		return map[string]any{}
	}
	return v.Export()
}

func toolResultJSON(r client.ToolResult, includePayloads bool) map[string]any {
	content := json.RawMessage("[]")
	if len(r.Content) > 0 {
		content = r.Content
	}
	out := map[string]any{
		"isError":    r.IsError,
		"content":    content,
		"durationMs": float64(r.Duration) / float64(time.Millisecond),
	}
	if len(r.StructuredContent) > 0 {
		out["structuredContent"] = r.StructuredContent
	}
	addOutcome(out, r.ServedBy, r.Cancelled, r.Err)
	_ = includePayloads // reserved: payloads are never attached to metric samples
	return out
}

// resourceResultJSON shapes a readResource result like a callTool result:
// {isError, contents, resource (the metric tag), durationMs, servedBy?,
// cancelled?, error?}.
func resourceResultJSON(r client.ResourceResult) map[string]any {
	contents := json.RawMessage("[]")
	if len(r.Contents) > 0 && string(r.Contents) != "null" {
		contents = r.Contents
	}
	out := map[string]any{
		"isError":    r.IsError,
		"contents":   contents,
		"resource":   r.Label,
		"durationMs": float64(r.Duration) / float64(time.Millisecond),
	}
	addOutcome(out, r.ServedBy, r.Cancelled, r.Err)
	return out
}

// promptResultJSON: {isError, description?, messages, durationMs,
// servedBy?, cancelled?, error?}.
func promptResultJSON(r client.PromptResult) map[string]any {
	messages := json.RawMessage("[]")
	if len(r.Messages) > 0 && string(r.Messages) != "null" {
		messages = r.Messages
	}
	out := map[string]any{
		"isError":    r.IsError,
		"messages":   messages,
		"durationMs": float64(r.Duration) / float64(time.Millisecond),
	}
	if r.Description != "" {
		out["description"] = r.Description
	}
	addOutcome(out, r.ServedBy, r.Cancelled, r.Err)
	return out
}

// addOutcome sets the servedBy, cancelled and error fields shared by all
// call results.
func addOutcome(out map[string]any, servedBy string, cancelled bool, err *client.Error) {
	if servedBy != "" {
		out["servedBy"] = servedBy
	}
	if cancelled {
		out["cancelled"] = true
	}
	if err != nil {
		e := map[string]any{"type": err.Type, "message": err.Message}
		if err.HTTPStatus != 0 {
			e["status"] = err.HTTPStatus
		}
		if err.Code != 0 {
			e["code"] = err.Code
		}
		if err.ServedBy != "" {
			e["servedBy"] = err.ServedBy
		}
		out["error"] = e
	}
}

// toJS converts Go data into native JS values (real arrays/objects) via
// JSON, so scripts can use Array methods and JSON.stringify freely.
func toJS(rt *sobek.Runtime, v any) sobek.Value {
	b, err := json.Marshal(v)
	if err != nil {
		common.Throw(rt, err)
	}
	parse, ok := sobek.AssertFunction(rt.Get("JSON").ToObject(rt).Get("parse"))
	if !ok {
		common.Throw(rt, errors.New("JSON.parse unavailable"))
	}
	res, err := parse(sobek.Undefined(), rt.ToValue(string(b)))
	if err != nil {
		common.Throw(rt, err)
	}
	return res
}

// throwMCP throws a JS Error carrying type/status/code properties.
func throwMCP(rt *sobek.Runtime, e *client.Error) {
	ctor, ok := sobek.AssertConstructor(rt.Get("Error"))
	if !ok {
		common.Throw(rt, e)
	}
	obj, err := ctor(nil, rt.ToValue(e.Error()))
	if err != nil {
		common.Throw(rt, e)
	}
	_ = obj.Set("type", e.Type)
	_ = obj.Set("status", e.HTTPStatus)
	if e.Code != 0 {
		_ = obj.Set("code", e.Code)
	}
	if e.ServedBy != "" {
		_ = obj.Set("servedBy", e.ServedBy)
	}
	panic(obj)
}
