package mcpload

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.k6.io/k6/v2/js/modulestest"
	"go.k6.io/k6/v2/metrics"

	"github.com/atul121001/mcpload/xk6-mcpload/client"
)

func TestProtocolOptionValidation(t *testing.T) {
	cases := []struct {
		opts    string
		wantErr string // "" = accepted
	}{
		{`{url: "http://x"}`, ""},
		{`{url: "http://x", protocol: "auto"}`, ""},
		{`{url: "http://x", protocol: "2026-07-28"}`, ""},
		{`{url: "http://x", protocol: "2025-06-18", fallbackVersion: "2025-03-26"}`, ""},
		{`{url: "http://x", protocol: "latest"}`, `invalid protocol "latest"`},
		{`{url: "http://x", protocol: "stateless"}`, `invalid protocol "stateless"`},
		{`{url: "http://x", protocol: "2025-6-18"}`, `invalid protocol`},
		{`{url: "http://x", protocol: 2025}`, `invalid protocol "2025"`},
		{`{url: "http://x", fallbackVersion: "v1"}`, `invalid fallbackVersion "v1"`},
	}
	for _, tc := range cases {
		t.Run(tc.opts, func(t *testing.T) {
			rt := modulestest.NewRuntime(t)
			m := New().NewModuleInstance(rt.VU).(*ModuleInstance)
			_ = rt.VU.Runtime().Set("mcp", m.Exports().Named)
			_, err := rt.RunOnEventLoop(`new mcp.Client(` + tc.opts + `)`)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "mcp.Client")):
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestParseAuthTimeout(t *testing.T) {
	cases := []struct {
		js   string
		want time.Duration
		ok   bool
	}{
		{"", 0, true}, {"'2s'", 2 * time.Second, true}, {"500", 500 * time.Millisecond, true},
		{"'0s'", 0, true}, {"'-1s'", 0, false}, {"'soon'", 0, false},
	}
	for _, tc := range cases {
		rt := modulestest.NewRuntime(t)
		extra := ""
		if tc.js != "" {
			extra = ", timeout: " + tc.js
		}
		val, err := rt.VU.Runtime().RunString(`({type: "oauth", tokenUrl: "http://tt", clientId: "a"` + extra + `})`)
		if err != nil {
			t.Fatal(err)
		}
		a, err := parseAuth(val.Export())
		if (err == nil) != tc.ok {
			t.Fatalf("%s: err=%v", tc.js, err)
		}
		if err == nil {
			if got := a.(*client.TokenSource).Config().Timeout; got != tc.want {
				t.Errorf("%s: Timeout=%v want %v", tc.js, got, tc.want)
			}
		}
	}
}

func newTestEmitter(t *testing.T, ctx context.Context, r *metrics.Registry, m *mcpMetrics, ch chan metrics.SampleContainer, vu string, runTags map[string]string) *emitter {
	t.Helper()
	return &emitter{
		ctx: ctx, samples: ch, m: m,
		tags:       r.RootTagSet().With("scenario", "s-"+vu).With("vu", vu).With("group", "::g"+vu),
		stableTags: m.root.WithTagsFromMap(runTags),
	}
}

func gaugeSamples(ch chan metrics.SampleContainer) []metrics.Sample {
	close(ch)
	var out []metrics.Sample
	for sc := range ch {
		for _, s := range sc.GetSamples() {
			if s.Metric.Name == "mcp_sessions_open" {
				out = append(out, s)
			}
		}
	}
	return out
}

// Concurrent opens/closes from many VUs produce gauge samples in counter
// order (each step is +-1 from the previous value) on one stable series.
func TestSessionsOpenGaugeOrderedAndStable(t *testing.T) {
	r := metrics.NewRegistry()
	m, err := registerMetrics(r)
	if err != nil {
		t.Fatal(err)
	}
	base := currentOpenSessions()
	ch := make(chan metrics.SampleContainer, 10000)
	runTags := map[string]string{"testid": "abc"}
	var wg sync.WaitGroup
	for vu := 0; vu < 16; vu++ {
		e := newTestEmitter(t, context.Background(), r, m, ch, string(rune('a'+vu)), runTags)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				e.OnSessionOpen()
				e.OnSessionClose()
			}
		}()
	}
	wg.Wait()
	ss := gaugeSamples(ch)
	if len(ss) != 16*50*2 {
		t.Fatalf("got %d gauge samples", len(ss))
	}
	prev := float64(base)
	for i, s := range ss {
		if d := s.Value - prev; d != 1 && d != -1 {
			t.Fatalf("sample %d: %v after %v (out of order)", i, s.Value, prev)
		}
		if i > 0 && s.Time.Before(ss[i-1].Time) {
			t.Fatalf("sample %d: time goes backwards", i)
		}
		if got := s.Tags.Map(); len(got) != 1 || got["testid"] != "abc" {
			t.Fatalf("gauge tags must be test-wide only, got %v", got)
		}
		prev = s.Value
	}
	if prev != float64(base) || currentOpenSessions() != base {
		t.Fatalf("final gauge %v, counter %d, want %d", prev, currentOpenSessions(), base)
	}
}

// close() after the VU context ended still decrements the process-wide
// counter (its sample is dropped); the next push carries the right value.
func TestSessionsOpenCloseAfterDone(t *testing.T) {
	r := metrics.NewRegistry()
	m, err := registerMetrics(r)
	if err != nil {
		t.Fatal(err)
	}
	base := currentOpenSessions()
	ch := make(chan metrics.SampleContainer, 100)
	ctx, cancel := context.WithCancel(context.Background())
	e := newTestEmitter(t, ctx, r, m, ch, "a", nil)
	e.OnSessionOpen()
	e.OnSessionOpen()
	cancel()
	e.OnSessionClose()
	if got := currentOpenSessions(); got != base+1 {
		t.Fatalf("counter=%d want %d", got, base+1)
	}
	other := newTestEmitter(t, context.Background(), r, m, ch, "b", nil)
	other.OnSessionClose()
	ss := gaugeSamples(ch)
	want := []float64{float64(base + 1), float64(base + 2), float64(base)}
	if len(ss) != len(want) {
		t.Fatalf("samples: %v", ss)
	}
	for i := range want {
		if ss[i].Value != want[i] {
			t.Fatalf("sample %d = %v want %v", i, ss[i].Value, want[i])
		}
	}
}

// Successful requests carry no error_type tag; failed ones keep it on every
// sample, including mcp_req_duration.
func TestErrorTypeTagOnlyOnFailures(t *testing.T) {
	cases := []struct {
		name      string
		errorType string
	}{
		{"success", ""},
		{"timeout", client.ErrTimeout},
		{"tool isError", client.ErrToolIsError},
		{"http", client.ErrHTTP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := metrics.NewRegistry()
			m, err := registerMetrics(r)
			if err != nil {
				t.Fatal(err)
			}
			ch := make(chan metrics.SampleContainer, 10)
			e := newTestEmitter(t, context.Background(), r, m, ch, "a", nil)
			e.OnRequest(client.RequestStats{Method: "tools/call", Tool: "echo", Status: 200, ErrorType: tc.errorType,
				Start: time.Now(), Duration: time.Millisecond, TTFB: time.Microsecond})
			close(ch)
			sawDuration := false
			for sc := range ch {
				for _, s := range sc.GetSamples() {
					v, has := s.Tags.Get("error_type")
					if tc.errorType == "" && has {
						t.Fatalf("%s: success sample has error_type=%q", s.Metric.Name, v)
					}
					if tc.errorType != "" && v != tc.errorType {
						t.Fatalf("%s: error_type=%q want %q", s.Metric.Name, v, tc.errorType)
					}
					if s.Metric.Name == "mcp_req_duration" {
						sawDuration = true
					}
				}
			}
			if !sawDuration {
				t.Fatal("no mcp_req_duration sample")
			}
		})
	}
}

func TestServerRequestSamples(t *testing.T) {
	r := metrics.NewRegistry()
	m, err := registerMetrics(r)
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan metrics.SampleContainer, 10)
	e := newTestEmitter(t, context.Background(), r, m, ch, "a", nil)
	e.OnServerRequest(client.ServerRequestStats{Method: "sampling/createMessage", Tool: "ask", Protocol: "2025-11-25",
		Status: 202, Start: time.Now(), Duration: 200 * time.Millisecond})
	e.OnServerRequest(client.ServerRequestStats{Method: "x/unknown", Status: 202, ErrorType: client.ErrUnsupportedRequest,
		Start: time.Now(), Duration: time.Millisecond})
	e.OnServerRequest(client.ServerRequestStats{Method: "sampling/createMessage", ErrorType: client.ErrUnsupportedRequest,
		NotAnswered: true, Start: time.Now()})
	close(ch)
	got := map[string]int{}
	for sc := range ch {
		for _, s := range sc.GetSamples() {
			got[s.Metric.Name]++
			if s.Metric.Name == "mcp_reqs" {
				t.Fatal("server requests must not count in mcp_reqs")
			}
			if s.Metric.Name == "mcp_server_request_duration" {
				if meth, _ := s.Tags.Get("method"); meth == "sampling/createMessage" {
					if tool, _ := s.Tags.Get("tool"); tool != "ask" {
						t.Fatalf("tool tag %q", tool)
					}
				}
			}
		}
	}
	want := map[string]int{"mcp_server_requests": 3, "mcp_server_request_duration": 2, "mcp_errors": 2}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: %d samples, want %d (all: %v)", k, got[k], v, got)
		}
	}
}

func TestResponderOptions(t *testing.T) {
	cases := []struct {
		kind, js string
		want     string // JSON result, or an error substring prefixed with "!"
		delay    time.Duration
	}{
		{"sampling", `true`, `{"content":{"text":"mcpload mock response","type":"text"},"model":"mcpload-mock","role":"assistant","stopReason":"endTurn"}`, 0},
		{"sampling", `({response: {role: 'assistant', content: {type: 'text', text: 'ok'}, model: 'x', stopReason: 'endTurn'}, delayMs: 200})`,
			`{"content":{"text":"ok","type":"text"},"model":"x","role":"assistant","stopReason":"endTurn"}`, 200 * time.Millisecond},
		{"elicitation", `({action: 'accept', content: {name: 'a'}, delayMs: '1s'})`, `{"action":"accept","content":{"name":"a"}}`, time.Second},
		{"elicitation", `({action: 'decline', content: {name: 'a'}})`, `{"action":"decline"}`, 0},
		{"elicitation", `({action: 'maybe'})`, "!action must be", 0},
		{"roots", `({roots: [{uri: 'file:///tmp', name: 'tmp'}]})`, `{"roots":[{"name":"tmp","uri":"file:///tmp"}]}`, 0},
		{"sampling", `({model: 'x'})`, `!unknown option "model"`, 0},
		{"sampling", `({delayMs: -5})`, "!must not be negative", 0},
		{"sampling", `'yes'`, "!must be true or an object", 0},
	}
	for _, tc := range cases {
		rt := modulestest.NewRuntime(t)
		v, err := rt.VU.Runtime().RunString(tc.js)
		if err != nil {
			t.Fatal(err)
		}
		r, err := parseResponder(tc.kind, v.Export())
		if strings.HasPrefix(tc.want, "!") {
			if err == nil || !strings.Contains(err.Error(), tc.want[1:]) {
				t.Errorf("%s %s: want error %q, got %v", tc.kind, tc.js, tc.want[1:], err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s %s: %v", tc.kind, tc.js, err)
		}
		if string(r.Result) != tc.want || r.Delay != tc.delay {
			t.Errorf("%s %s: got %s / %v", tc.kind, tc.js, r.Result, r.Delay)
		}
	}
	if r, err := parseResponder("sampling", false); r != nil || err != nil {
		t.Fatalf("false: %v %v", r, err)
	}
}
