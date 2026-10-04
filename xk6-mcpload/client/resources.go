package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Resources and prompts (server features). The client declares no
// capability for them: they are offered by the server. In stateless mode
// (2026-07-28) every request carries Mcp-Method; resources/read sends
// params.uri and prompts/get params.name as Mcp-Name (base64-wrapped by
// EncodeHeaderValue when needed), as the spec's "Standard Request Headers"
// table says. The list methods carry no Mcp-Name.
//
// Metric tags: resources/read is tagged `resource` and prompts/get `prompt`
// (never `tool`). A resource's tag is the server-declared name of its URI
// from resources/list (or of the template from resources/templates/list it
// matches) when this session listed them; otherwise a bounded fallback built
// from the URI's scheme, host and first path segment (resourceFallbackLabel).
// Each kind keeps at most MaxResourceLabels / MaxPromptLabels distinct
// values per process; later new values are tagged OtherLabel, so a server
// with thousands of resources cannot blow up metric cardinality.

const (
	// MaxResourceLabels bounds the distinct `resource` tag values per process.
	MaxResourceLabels = 50
	// MaxPromptLabels bounds the distinct `prompt` tag values per process.
	MaxPromptLabels = 50
	// OtherLabel is the tag value once a kind's label budget is used up.
	OtherLabel = "other"
)

// Resource is one entry of a resources/list result.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Size        *int64 `json:"size,omitempty"`
}

// ResourceTemplate is one entry of a resources/templates/list result.
type ResourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// Prompt is one entry of a prompts/list result.
type Prompt struct {
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// PromptArgument is one argument a prompt accepts.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// ResourceResult is the outcome of a resources/read. Like ToolResult it
// never carries a Go error: Err is set on any failure (IsError mirrors it).
// Contents is the raw `contents` array ({uri, mimeType?, text | blob}).
type ResourceResult struct {
	IsError   bool
	Contents  json.RawMessage
	Label     string // the `resource` tag value the request was recorded with
	Duration  time.Duration
	ServedBy  string
	Err       *Error
	Cancelled bool
}

// PromptResult is the outcome of a prompts/get; see ResourceResult.
// Messages is the raw `messages` array ({role, content}).
type PromptResult struct {
	IsError     bool
	Description string
	Messages    json.RawMessage
	Duration    time.Duration
	ServedBy    string
	Err         *Error
	Cancelled   bool
}

// resourceIndex remembers what this session listed, to tag reads by the
// server-declared names.
type resourceIndex struct {
	mu        sync.RWMutex
	names     map[string]string // uri -> name
	templates []templateMatcher
}

type templateMatcher struct {
	re   *regexp.Regexp
	name string
}

func (ri *resourceIndex) addResources(rs []Resource) {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	if ri.names == nil {
		ri.names = map[string]string{}
	}
	for _, r := range rs {
		if r.URI != "" && r.Name != "" {
			ri.names[r.URI] = r.Name
		}
	}
}

func (ri *resourceIndex) addTemplates(ts []ResourceTemplate) {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	for _, t := range ts {
		if t.Name == "" {
			continue
		}
		if re := templateRegexp(t.URITemplate); re != nil {
			ri.templates = append(ri.templates, templateMatcher{re: re, name: t.Name})
		}
	}
}

// name returns the server-declared name for uri ("" when unknown).
func (ri *resourceIndex) name(uri string) string {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	if n := ri.names[uri]; n != "" {
		return n
	}
	for _, t := range ri.templates {
		if t.re.MatchString(uri) {
			return t.name
		}
	}
	return ""
}

// templateRegexp turns an RFC 6570 URI template into a matching regexp: a
// simple {var} matches one path segment, any other expression ({+var},
// {/var}, {?q}, ...) matches anything. nil when the template is malformed.
func templateRegexp(tpl string) *regexp.Regexp {
	if tpl == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString("^")
	for rest := tpl; rest != ""; {
		i := strings.IndexByte(rest, '{')
		if i < 0 {
			b.WriteString(regexp.QuoteMeta(rest))
			break
		}
		b.WriteString(regexp.QuoteMeta(rest[:i]))
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 {
			return nil
		}
		expr := rest[i+1 : i+j]
		if expr != "" && strings.ContainsRune("+#./;?&", rune(expr[0])) {
			b.WriteString(".*")
		} else {
			b.WriteString("[^/?#]*")
		}
		rest = rest[i+j+1:]
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}

// resourceFallbackLabel tags a URI this session has no name for: scheme and
// host, plus the first path segment when the path has more segments
// ("file:///projects/app/a.json" -> "file:///projects",
// "demo://items/42" -> "demo://items", "https://x.test/readme" ->
// "https://x.test"). User info, query and fragment are never kept (they may
// hold credentials or ids). Unparseable URIs are OtherLabel.
func resourceFallbackLabel(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme == "" {
		return OtherLabel
	}
	path := u.Path
	if u.Opaque != "" {
		path = u.Opaque
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	label := u.Scheme + "://" + u.Host
	if u.Opaque != "" {
		label = u.Scheme + ":"
	}
	if len(segs) > 1 && segs[0] != "" {
		if u.Opaque != "" {
			label += segs[0]
		} else {
			label += "/" + segs[0]
		}
	}
	return label
}

// labelSet hands out at most max distinct labels; later new ones get
// OtherLabel. Shared by all sessions of the process.
type labelSet struct {
	mu   sync.Mutex
	seen map[string]bool
	max  int
}

func (l *labelSet) get(v string) string {
	if v == "" {
		return OtherLabel
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen[v] {
		return v
	}
	if len(l.seen) >= l.max {
		return OtherLabel
	}
	if l.seen == nil {
		l.seen = map[string]bool{}
	}
	l.seen[v] = true
	return v
}

var (
	resourceLabels = &labelSet{max: MaxResourceLabels}
	promptLabels   = &labelSet{max: MaxPromptLabels}
)

// ResourceLabel returns the `resource` tag value for a read of uri on this
// session (see the package comment above).
func (s *Session) ResourceLabel(uri string) string {
	n := s.res.name(uri)
	if n == "" {
		n = resourceFallbackLabel(uri)
	}
	return resourceLabels.get(n)
}

// labelObserver adds the resource/prompt tag to the request events of one
// operation and forwards everything else unchanged.
type labelObserver struct {
	next             Observer
	resource, prompt string
}

func (o *labelObserver) OnRequest(st RequestStats) {
	st.Resource, st.Prompt = o.resource, o.prompt
	o.next.OnRequest(st)
}
func (o *labelObserver) OnConnect(st ConnectStats)  { o.next.OnConnect(st) }
func (o *labelObserver) OnTokenFetch(st TokenStats) { o.next.OnTokenFetch(st) }
func (o *labelObserver) OnSessionOpen()             { o.next.OnSessionOpen() }
func (o *labelObserver) OnSessionClose()            { o.next.OnSessionClose() }
func (o *labelObserver) OnCancel(st CancelStats) {
	if n, ok := o.next.(CancelObserver); ok {
		n.OnCancel(st)
	}
}
func (o *labelObserver) OnServerRequest(st ServerRequestStats) {
	if n, ok := o.next.(ServerRequestObserver); ok {
		n.OnServerRequest(st)
	}
}

// listPaged sends method until nextCursor runs out, decoding each page's
// key array into out (appended).
func listPaged[T any](ctx context.Context, s *Session, method, key string) ([]T, error) {
	var all []T
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 10000; page++ {
		var params map[string]any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		raw, err := s.Request(ctx, method, "", params)
		if err != nil {
			return all, err
		}
		var lr map[string]json.RawMessage
		if uerr := json.Unmarshal(raw, &lr); uerr != nil {
			return all, &Error{Type: ErrJSONRPC, Message: "decoding " + method + " result: " + uerr.Error()}
		}
		var items []T
		if b := lr[key]; len(b) > 0 && string(b) != "null" {
			if uerr := json.Unmarshal(b, &items); uerr != nil {
				return all, &Error{Type: ErrJSONRPC, Message: fmt.Sprintf("decoding %s result.%s: %v", method, key, uerr)}
			}
		}
		all = append(all, items...)
		var next string
		if b := lr["nextCursor"]; len(b) > 0 {
			_ = json.Unmarshal(b, &next)
		}
		if next == "" || seen[next] {
			return all, nil
		}
		seen[next] = true
		cursor = next
	}
	return all, nil
}

// ListResources returns all resources (resources/list, following
// nextCursor) and remembers their names for the `resource` tag of later
// reads on this session.
func (s *Session) ListResources(ctx context.Context) ([]Resource, error) {
	rs, err := listPaged[Resource](ctx, s, "resources/list", "resources")
	s.res.addResources(rs)
	return rs, err
}

// ListResourceTemplates returns all resource templates
// (resources/templates/list, following nextCursor); reads of URIs that
// match one are tagged with its name.
func (s *Session) ListResourceTemplates(ctx context.Context) ([]ResourceTemplate, error) {
	ts, err := listPaged[ResourceTemplate](ctx, s, "resources/templates/list", "resourceTemplates")
	s.res.addTemplates(ts)
	return ts, err
}

// ListPrompts returns all prompts (prompts/list, following nextCursor).
func (s *Session) ListPrompts(ctx context.Context) ([]Prompt, error) {
	return listPaged[Prompt](ctx, s, "prompts/list", "prompts")
}

// ReadResource sends resources/read. Failures are reported in the result's
// Err, never as a Go error.
func (s *Session) ReadResource(ctx context.Context, uri string) ResourceResult {
	return s.ReadResourceWith(ctx, uri, nil, CallOptions{})
}

// ReadResourceWith is ReadResource with params._meta (when not empty) and
// per-call options (CancelAfter, as for CallToolWith).
func (s *Session) ReadResourceWith(ctx context.Context, uri string, meta map[string]any, co CallOptions) ResourceResult {
	label := s.ResourceLabel(uri)
	params := map[string]any{"uri": uri}
	if len(meta) > 0 {
		params["_meta"] = meta
	}
	ctx = WithObserver(ctx, &labelObserver{next: s.obs(ctx), resource: label})
	r := s.request(ctx, "resources/read", uri, params, false, co)
	out := ResourceResult{Label: label, Duration: r.stats.Duration, ServedBy: r.servedBy}
	if r.err != nil {
		out.IsError, out.Err, out.Cancelled = true, r.err, r.err.Type == ErrCancelled
		return out
	}
	var rr struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(r.result, &rr); err != nil {
		out.IsError = true
		out.Err = &Error{Type: ErrJSONRPC, HTTPStatus: r.stats.Status, Message: "decoding resources/read result: " + err.Error(), ServedBy: r.servedBy}
		return out
	}
	out.Contents = rr.Contents
	return out
}

// GetPrompt sends prompts/get. Failures are reported in the result's Err,
// never as a Go error.
func (s *Session) GetPrompt(ctx context.Context, name string, args map[string]string) PromptResult {
	return s.GetPromptWith(ctx, name, args, nil, CallOptions{})
}

// GetPromptWith is GetPrompt with params._meta (when not empty) and per-call
// options.
func (s *Session) GetPromptWith(ctx context.Context, name string, args map[string]string, meta map[string]any, co CallOptions) PromptResult {
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = args
	}
	if len(meta) > 0 {
		params["_meta"] = meta
	}
	ctx = WithObserver(ctx, &labelObserver{next: s.obs(ctx), prompt: promptLabels.get(name)})
	r := s.request(ctx, "prompts/get", name, params, false, co)
	out := PromptResult{Duration: r.stats.Duration, ServedBy: r.servedBy}
	if r.err != nil {
		out.IsError, out.Err, out.Cancelled = true, r.err, r.err.Type == ErrCancelled
		return out
	}
	var pr struct {
		Description string          `json:"description"`
		Messages    json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(r.result, &pr); err != nil {
		out.IsError = true
		out.Err = &Error{Type: ErrJSONRPC, HTTPStatus: r.stats.Status, Message: "decoding prompts/get result: " + err.Error(), ServedBy: r.servedBy}
		return out
	}
	out.Description, out.Messages = pr.Description, pr.Messages
	return out
}

// Call kinds of a mixed parallel batch (Session.Parallel).
const (
	KindTool     = ""
	KindResource = "resource"
	KindPrompt   = "prompt"
)

// Call is one element of a mixed parallel batch: a tools/call (Kind
// KindTool: Name, Args), a resources/read (KindResource: URI) or a
// prompts/get (KindPrompt: Name, PromptArgs).
type Call struct {
	Kind       string
	Name       string
	URI        string
	Args       any
	PromptArgs map[string]string
	Meta       map[string]any
	CallOptions
}

// CallResult holds the result of a Call; only the field of its Kind is set.
type CallResult struct {
	Kind     string
	Tool     ToolResult
	Resource ResourceResult
	Prompt   PromptResult
}

// Parallel runs a mixed batch of tool calls, resource reads and prompt gets
// concurrently (like CallParallel) and returns results in input order.
func (s *Session) Parallel(ctx context.Context, calls []Call) []CallResult {
	out := make([]CallResult, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func(i int, c Call) {
			defer wg.Done()
			out[i].Kind = c.Kind
			switch c.Kind {
			case KindResource:
				out[i].Resource = s.ReadResourceWith(ctx, c.URI, c.Meta, c.CallOptions)
			case KindPrompt:
				out[i].Prompt = s.GetPromptWith(ctx, c.Name, c.PromptArgs, c.Meta, c.CallOptions)
			default:
				out[i].Tool = s.CallToolMetaWith(ctx, c.Name, c.Args, c.Meta, c.CallOptions)
			}
		}(i, c)
	}
	wg.Wait()
	return out
}
