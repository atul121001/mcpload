package mcpload

import (
	"context"
	"strconv"
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
}

func registerMetrics(r *metrics.Registry) (*mcpMetrics, error) {
	m := &mcpMetrics{}
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
	return m, err
}

// openSessions counts client-side open sessions across all VUs.
var openSessions atomic.Int64

// emitter implements client.Observer and pushes k6 samples. It is created on
// the JS thread (capturing the VU's current tags) and is safe to use from the
// goroutines spawned by callParallel: it only touches the samples channel.
type emitter struct {
	ctx     context.Context
	samples chan<- metrics.SampleContainer
	m       *mcpMetrics
	tags    *metrics.TagSet
	meta    map[string]string
}

var _ client.Observer = (*emitter)(nil)

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
	metrics.PushIfNotDone(e.ctx, e.samples, metrics.ConnectedSamples{Samples: ss, Tags: tags, Time: t})
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

func (e *emitter) OnRequest(st client.RequestStats) {
	tags := withTag(e.tags, "method", st.Method)
	tags = withTag(tags, "tool", st.Tool)
	tags = withTag(tags, "protocol", st.Protocol)
	tags = withTag(tags, "status", statusTag(st.Status))
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
	if st.ErrorType != "" {
		ss = append(ss, sample(e.m.Errors, 1))
	}
	if st.Method == "tools/call" {
		ss = append(ss, sample(e.m.ToolErrorRate, metrics.B(st.ErrorType != "")))
	}
	e.push(tags, end, ss...)
}

func (e *emitter) OnConnect(st client.ConnectStats) {
	tags := withTag(e.tags, "method", st.Method)
	tags = withTag(tags, "protocol", st.Protocol)
	tags = withTag(tags, "status", statusTag(st.Status))
	tags = withTag(tags, "error_type", st.ErrorType)
	e.push(tags, st.Start.Add(st.Duration), sample(e.m.ConnectDuration, metrics.D(st.Duration)))
}

func (e *emitter) OnTokenFetch(st client.TokenStats) {
	tags := withTag(e.tags, "method", "oauth/token")
	tags = withTag(tags, "status", statusTag(st.Status))
	tags = withTag(tags, "error_type", st.ErrorType)
	// A failed fetch is counted in mcp_errors by the request that needed the
	// token (error_type auth), so it is not counted twice here.
	e.push(tags, st.Start.Add(st.Duration), sample(e.m.OAuthDuration, metrics.D(st.Duration)))
}

func (e *emitter) OnSessionOpen() {
	n := openSessions.Add(1)
	e.push(e.tags, time.Now(), sample(e.m.SessionsOpen, float64(n)))
}

func (e *emitter) OnSessionClose() {
	n := openSessions.Add(-1)
	e.push(e.tags, time.Now(), sample(e.m.SessionsOpen, float64(n)))
}
