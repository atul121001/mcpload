// Package mcpload is a k6 extension (JS import "k6/x/mcpload") for load
// testing remote MCP servers over streamable HTTP.
package mcpload

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
		case "includePayloads":
			b, _ := val.(bool)
			c.includePayloads = b
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

// emitter captures the VU's current tags on the JS thread.
func (mi *ModuleInstance) emitter() *emitter {
	state := mi.vu.State()
	tm := state.Tags.GetCurrentValues()
	return &emitter{
		ctx:     mi.vu.Context(),
		samples: state.Samples,
		m:       mi.metrics,
		tags:    tm.Tags,
		meta:    tm.Metadata,
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
	must(rt, obj.Set("listTools", js.listTools))
	must(rt, obj.Set("callTool", js.callTool))
	must(rt, obj.Set("callParallel", js.callParallel))
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

func (js *jsSession) callTool(name string, args sobek.Value) sobek.Value {
	rt := js.mi.vu.Runtime()
	a := exportArgs(args)
	ctx := client.WithObserver(js.mi.vu.Context(), js.ctx())
	r := js.s.CallTool(ctx, name, a)
	return toJS(rt, toolResultJSON(r, js.includePayloads))
}

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
	calls := make([]client.ToolCall, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok || str(m["name"]) == "" {
			common.Throw(rt, fmt.Errorf("callParallel: element %d must be {name, args}", i))
		}
		calls[i] = client.ToolCall{Name: str(m["name"]), Args: m["args"]}
	}
	ctx := client.WithObserver(js.mi.vu.Context(), js.ctx())
	results := js.s.CallParallel(ctx, calls) // returns after all goroutines finish
	out := make([]map[string]any, len(results))
	for i, r := range results {
		out[i] = toolResultJSON(r, js.includePayloads)
	}
	return toJS(rt, out)
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
	if r.Err != nil {
		e := map[string]any{"type": r.Err.Type, "message": r.Err.Message}
		if r.Err.HTTPStatus != 0 {
			e["status"] = r.Err.HTTPStatus
		}
		if r.Err.Code != 0 {
			e["code"] = r.Err.Code
		}
		out["error"] = e
	}
	_ = includePayloads // reserved: payloads are never attached to metric samples
	return out
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
	panic(obj)
}
