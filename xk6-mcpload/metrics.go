package mcpload

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.k6.io/k6/v2/metrics"

	"github.com/atul121001/mcpload/xk6-mcpload/client"
)

// mcpMetrics holds the custom metrics. They are registered through
// vu.InitEnv().Registry (common.InitEnvironment embeds *lib.TestPreInitState,
// which carries the *metrics.Registry). Registry.NewMetric is idempotent for
// the same name/type, so every VU gets the same *metrics.Metric.
type mcpMetrics struct {
	ReqDuration     *metrics.Metric
	ReqTTFB         *metrics.Metric
	StreamDuration  *metrics.Metric
	ConnectDuration *metrics.Metric
	OAuthDuration   *metrics.Metric
	Reqs            *metrics.Metric
	Errors          *metrics.Metric
	ToolErrorRate   *metrics.Metric
	SessionsOpen    *metrics.Metric
	ServerReqs      *metrics.Metric
	ServerReqDur    *metrics.Metric
	Cancellations   *metrics.Metric
	CancelDuration  *metrics.Metric
	LateResponse    *metrics.Metric
	SpawnDuration   *metrics.Metric
	ProcessesOpen   *metrics.Metric
	StdoutInvalid   *metrics.Metric
	ProcessExits    *metrics.Metric

	root *metrics.TagSet // the registry root tag set (no tags)
}

func registerMetrics(r *metrics.Registry) (*mcpMetrics, error) {
	m := &mcpMetrics{root: r.RootTagSet()}
	var err error
	reg := func(dst **metrics.Metric, name string, typ metrics.MetricType, vt ...metrics.ValueType) {
		if err != nil {
			return
		}
		*dst, err = r.NewMetric(name, typ, vt...)
	}
	reg(&m.ReqDuration, "mcp_req_duration", metrics.Trend, metrics.Time)
	reg(&m.ReqTTFB, "mcp_req_ttfb", metrics.Trend, metrics.Time)
	reg(&m.StreamDuration, "mcp_stream_duration", metrics.Trend, metrics.Time)
	reg(&m.ConnectDuration, "mcp_connect_duration", metrics.Trend, metrics.Time)
	reg(&m.OAuthDuration, "mcp_oauth_refresh_duration", metrics.Trend, metrics.Time)
	reg(&m.Reqs, "mcp_reqs", metrics.Counter)
	reg(&m.Errors, "mcp_errors", metrics.Counter)
	reg(&m.ToolErrorRate, "mcp_tool_error_rate", metrics.Rate)
	reg(&m.SessionsOpen, "mcp_sessions_open", metrics.Gauge)
	reg(&m.ServerReqs, "mcp_server_requests", metrics.Counter)
	reg(&m.ServerReqDur, "mcp_server_request_duration", metrics.Trend, metrics.Time)
	reg(&m.Cancellations, "mcp_cancellations", metrics.Counter)
	reg(&m.CancelDuration, "mcp_cancel_duration", metrics.Trend, metrics.Time)
	reg(&m.LateResponse, "mcp_cancel_late_response", metrics.Trend, metrics.Time)
	reg(&m.SpawnDuration, "mcp_process_spawn_duration", metrics.Trend, metrics.Time)
	reg(&m.ProcessesOpen, "mcp_processes_open", metrics.Gauge)
	reg(&m.StdoutInvalid, "mcp_stdout_invalid_lines", metrics.Counter)
	reg(&m.ProcessExits, "mcp_process_exits", metrics.Counter)
	return m, err
}

// processGauge is a process-wide count (all VUs and scenarios together)
// reported as a gauge. Every change is pushed as a sample while mu is held,
// so samples reach k6's sample channel in the same order as the counter
// changes and the gauge's last value is the current count. The counter
// itself is always updated, even when the sample cannot be pushed because the
// VU's context has ended; the next push from any VU then carries the correct
// value.
type processGauge struct {
	mu sync.Mutex
	n  int64
}

func (g *processGauge) get() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}

var (
	// openSessions backs mcp_sessions_open: client-side open MCP sessions.
	openSessions processGauge
	// openProcesses backs mcp_processes_open: live stdio server processes.
	openProcesses processGauge
)

// emitter implements client.Observer and pushes k6 samples. It is created on
// the JS thread (capturing the VU's current tags) and is safe to use from the
// goroutines spawned by callParallel: it only touches the samples channel.
type emitter struct {
	ctx     context.Context
	samples chan<- metrics.SampleContainer
	m       *mcpMetrics
	tags    *metrics.TagSet
	meta    map[string]string
	// stableTags tags the process-wide mcp_sessions_open and
	// mcp_processes_open gauges: only the test-wide tags (options.tags),
	// never per-VU, scenario or group tags, so all VUs push to one time
	// series.
	stableTags *metrics.TagSet
	// transport is the `transport` tag of the session the emitter reports
	// for: client.TransportStdio or client.TransportHTTP ("" = http).
	transport string
}

var (
	_ client.Observer              = (*emitter)(nil)
	_ client.ServerRequestObserver = (*emitter)(nil)
	_ client.CancelObserver        = (*emitter)(nil)
	_ client.ProcessObserver       = (*emitter)(nil)
	_ client.ProcessObserver       = (*procObserver)(nil)
)

func withTag(ts *metrics.TagSet, k, v string) *metrics.TagSet {
	if v == "" {
		return ts
	}
	return ts.With(k, v)
}

func (e *emitter) push(tags *metrics.TagSet, t time.Time, ss ...metrics.Sample) {
	for i := range ss {
		ss[i].Tags = tags
		ss[i].Time = t
		ss[i].Metadata = e.meta
	}
	pushSafe(e.ctx, e.samples, metrics.ConnectedSamples{Samples: ss, Tags: tags, Time: t})
}

// pushSafe sends a sample container unless ctx (the VU's iteration context)
// has ended. Some events arrive from the client's background goroutines
// after the JS call that caused them returned (a late cancellation outcome,
// a stdio process exit or stdout line), possibly after the iteration or the
// whole test ended. Unlike metrics.PushIfNotDone, pushSafe stops waiting on a
// full channel once ctx is done, and a send on the samples channel that k6
// closed at the end of the test is dropped instead of panicking in a
// goroutine nobody recovers.
func pushSafe(ctx context.Context, ch chan<- metrics.SampleContainer, sc metrics.SampleContainer) (ok bool) {
	if ctx.Err() != nil {
		return false
	}
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	select {
	case ch <- sc:
		return true
	case <-ctx.Done():
		return false
	}
}

func sample(m *metrics.Metric, v float64) metrics.Sample {
	return metrics.Sample{TimeSeries: metrics.TimeSeries{Metric: m}, Value: v}
}

func statusTag(s int) string {
	if s == 0 {
		return "0"
	}
	return strconv.Itoa(s)
}

// baseTags returns the VU tags plus `transport`: transport when set (from
// the event's stats), else the emitter's own, else http.
func (e *emitter) baseTags(transport string) (*metrics.TagSet, string) {
	if transport == "" {
		transport = e.transport
	}
	if transport == "" {
		transport = client.TransportHTTP
	}
	return e.tags.With("transport", transport), transport
}

// withStatus adds the `status` tag over HTTP. Over stdio there is no HTTP
// status (it is always 0), so the tag is omitted.
func withStatus(ts *metrics.TagSet, transport string, status int) *metrics.TagSet {
	if transport == client.TransportStdio {
		return ts
	}
	return withTag(ts, "status", statusTag(status))
}

// OnRequest emits the per-request samples. resources/read is tagged
// `resource` and prompts/get `prompt` (bounded values, see
// client/resources.go). A successful request carries no
// error_type tag at all (empty tags are omitted); a failed one keeps its
// error_type tag on every sample, including mcp_req_duration, so success-only
// percentiles can be computed by selecting samples without the tag.
func (e *emitter) OnRequest(st client.RequestStats) {
	tags, tr := e.baseTags(st.Transport)
	tags = withTag(tags, "method", st.Method)
	tags = withTag(tags, "tool", st.Tool)
	tags = withTag(tags, "resource", st.Resource)
	tags = withTag(tags, "prompt", st.Prompt)
	tags = withTag(tags, "protocol", st.Protocol)
	tags = withStatus(tags, tr, st.Status)
	tags = withTag(tags, "error_type", st.ErrorType)
	end := st.Start.Add(st.Duration)
	ss := []metrics.Sample{sample(e.m.Reqs, 1)}
	if !st.NotSent {
		ss = append(ss, sample(e.m.ReqDuration, metrics.D(st.Duration)))
	}
	if st.Status != 0 {
		ss = append(ss, sample(e.m.ReqTTFB, metrics.D(st.TTFB)))
	}
	if st.Streamed {
		ss = append(ss, sample(e.m.StreamDuration, metrics.D(st.Stream)))
	}
	// A call the script cancelled itself (error_type cancelled) is not a
	// server failure: it is left out of mcp_errors and mcp_tool_error_rate.
	cancelled := st.ErrorType == client.ErrCancelled
	if st.ErrorType != "" && !cancelled {
		ss = append(ss, sample(e.m.Errors, 1))
	}
	if st.Method == "tools/call" && !cancelled {
		ss = append(ss, sample(e.m.ToolErrorRate, metrics.B(st.ErrorType != "")))
	}
	e.push(tags, end, ss...)
}

// OnCancel emits mcp_cancellations (tagged `reason` and `outcome`; `status`
// is the notification POST's), mcp_cancel_duration for every cancellation
// that was sent and mcp_cancel_late_response for a late response. A failed
// notification is already counted in mcp_errors by its own request.
func (e *emitter) OnCancel(st client.CancelStats) {
	tags, tr := e.baseTags("")
	tags = withTag(tags, "method", st.Method)
	tags = withTag(tags, "tool", st.Tool)
	tags = withTag(tags, "protocol", st.Protocol)
	tags = withTag(tags, "reason", st.Reason)
	tags = withTag(tags, "outcome", st.Outcome)
	tags = withTag(tags, "error_type", st.ErrorType)
	if st.Outcome != client.CancelOutcomeCompleted {
		tags = withStatus(tags, tr, st.Status)
	}
	ss := []metrics.Sample{sample(e.m.Cancellations, 1)}
	if st.Outcome != client.CancelOutcomeCompleted && st.Outcome != client.CancelOutcomeSendFailed {
		ss = append(ss, sample(e.m.CancelDuration, metrics.D(st.Duration)))
	}
	if st.Outcome == client.CancelOutcomeLateResponse {
		ss = append(ss, sample(e.m.LateResponse, metrics.D(st.LateAfter)))
	}
	e.push(tags, st.Start.Add(st.Duration), ss...)
}

// OnServerRequest emits the samples for a server-to-client request read from
// a response stream (or from stdout over stdio). `method` is the server's
// method, `tool` the tool whose stream carried it and `status` the status of
// the answer POST (HTTP only). They are not counted in mcp_reqs: the answer
// is not a client request.
func (e *emitter) OnServerRequest(st client.ServerRequestStats) {
	tags, tr := e.baseTags("")
	tags = withTag(tags, "method", st.Method)
	tags = withTag(tags, "tool", st.Tool)
	tags = withTag(tags, "protocol", st.Protocol)
	tags = withStatus(tags, tr, st.Status)
	tags = withTag(tags, "error_type", st.ErrorType)
	ss := []metrics.Sample{sample(e.m.ServerReqs, 1)}
	if !st.NotAnswered {
		ss = append(ss, sample(e.m.ServerReqDur, metrics.D(st.Duration)))
	}
	if st.ErrorType != "" {
		ss = append(ss, sample(e.m.Errors, 1))
	}
	e.push(tags, st.Start.Add(st.Duration), ss...)
}

func (e *emitter) OnConnect(st client.ConnectStats) {
	tags, tr := e.baseTags(st.Transport)
	tags = withTag(tags, "method", st.Method)
	tags = withTag(tags, "protocol", st.Protocol)
	tags = withStatus(tags, tr, st.Status)
	tags = withTag(tags, "error_type", st.ErrorType)
	e.push(tags, st.Start.Add(st.Duration), sample(e.m.ConnectDuration, metrics.D(st.Duration)))
}

func (e *emitter) OnTokenFetch(st client.TokenStats) {
	tags, _ := e.baseTags(client.TransportHTTP) // the token endpoint is always HTTP
	tags = withTag(tags, "method", "oauth/token")
	tags = withTag(tags, "status", statusTag(st.Status))
	tags = withTag(tags, "error_type", st.ErrorType)
	// A failed fetch is counted in mcp_errors by the request that needed the
	// token (error_type auth), so it is not counted twice here.
	e.push(tags, st.Start.Add(st.Duration), sample(e.m.OAuthDuration, metrics.D(st.Duration)))
}

func (e *emitter) OnSessionOpen()  { e.gaugeDelta(&openSessions, e.m.SessionsOpen, 1) }
func (e *emitter) OnSessionClose() { e.gaugeDelta(&openSessions, e.m.SessionsOpen, -1) }

func (e *emitter) gaugeDelta(g *processGauge, m *metrics.Metric, d int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n += d
	tags := e.stableTags
	if tags == nil {
		tags = e.tags
	}
	s := sample(m, float64(g.n))
	s.Tags, s.Time = tags, time.Now()
	pushSafe(e.ctx, e.samples, s)
}

// currentOpenSessions returns the process-wide open session count.
func currentOpenSessions() int64 { return openSessions.get() }

// currentOpenProcesses returns the process-wide live stdio process count.
func currentOpenProcesses() int64 { return openProcesses.get() }

// OnProcessSpawn emits mcp_process_spawn_duration (with error_type
// process_spawn when the process did not start) and, for a started process,
// writes its PID file and raises mcp_processes_open.
func (e *emitter) OnProcessSpawn(st client.ProcessStats) {
	tags, _ := e.baseTags(client.TransportStdio)
	tags = withTag(tags, "error_type", st.ErrorType)
	e.push(tags, st.Start.Add(st.Spawn), sample(e.m.SpawnDuration, metrics.D(st.Spawn)))
	if st.ErrorType == "" && st.PID > 0 {
		writePIDFile(st.PID)
		e.gaugeDelta(&openProcesses, e.m.ProcessesOpen, 1)
	}
}

// OnProcessExit counts mcp_process_exits (tags `expected` — the exit
// followed close() or a failed connect — and `exit_code`, -1 when killed by
// a signal), removes the PID file and lowers mcp_processes_open.
func (e *emitter) OnProcessExit(st client.ProcessExitStats) {
	removePIDFile(st.PID)
	e.gaugeDelta(&openProcesses, e.m.ProcessesOpen, -1)
	tags, _ := e.baseTags(client.TransportStdio)
	tags = tags.With("expected", strconv.FormatBool(st.Expected)).With("exit_code", strconv.Itoa(st.ExitCode))
	e.push(tags, time.Now(), sample(e.m.ProcessExits, 1))
}

// OnStdoutInvalid counts a line on the server's stdout that is not a
// JSON-RPC message (mcp_stdout_invalid_lines).
func (e *emitter) OnStdoutInvalid(client.StdoutInvalidStats) {
	tags, _ := e.baseTags(client.TransportStdio)
	e.push(tags, time.Now(), sample(e.m.StdoutInvalid, 1))
}

// procObserver is the Observer given to client.Connect for a stdio session.
// It reports the connect itself through the emitter of the connect() call,
// and forwards process events, which the client delivers from its own
// goroutines at any time, to the emitter of the latest JS call on the
// session (set by jsSession.ctx). An exit or stdout line seen during a later
// iteration is then tagged and pushed with that iteration's context instead
// of the (ended) context of the iteration that connected. Events after the
// iteration of the latest call ended are dropped (the gauge's counter is
// still updated).
type procObserver struct {
	*emitter
	cur atomic.Pointer[emitter]
}

func newProcObserver(e *emitter) *procObserver {
	p := &procObserver{emitter: e}
	p.cur.Store(e)
	return p
}

func (p *procObserver) use(e *emitter) { p.cur.Store(e) }

func (p *procObserver) OnProcessSpawn(st client.ProcessStats) { p.cur.Load().OnProcessSpawn(st) }
func (p *procObserver) OnProcessExit(st client.ProcessExitStats) {
	p.cur.Load().OnProcessExit(st)
}
func (p *procObserver) OnStdoutInvalid(st client.StdoutInvalidStats) {
	p.cur.Load().OnStdoutInvalid(st)
}

// pidDirEnv names a directory in which a file named after the PID of every
// live stdio server process is kept (content: the PID and a newline), so
// that a supervisor (the mcpload CLI) can sample their CPU and memory. The
// file is written after the spawn and removed on exit. Best-effort: errors
// are ignored.
const pidDirEnv = "MCPLOAD_PID_DIR"

func pidFile(pid int) string {
	dir := os.Getenv(pidDirEnv)
	if dir == "" || pid <= 0 {
		return ""
	}
	return filepath.Join(dir, strconv.Itoa(pid))
}

func writePIDFile(pid int) {
	if f := pidFile(pid); f != "" {
		_ = os.WriteFile(f, []byte(strconv.Itoa(pid)+"\n"), 0o644)
	}
}

func removePIDFile(pid int) {
	if f := pidFile(pid); f != "" {
		_ = os.Remove(f)
	}
}
