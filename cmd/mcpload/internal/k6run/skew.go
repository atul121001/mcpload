package k6run

// Custom metrics of scenarios/version-skew.js.
const (
	MetricSkewRequests        = "mcp_skew_requests"
	MetricSkewFailures        = "mcp_skew_failures"
	MetricSkewFailureDuration = "mcp_skew_failure_duration"
	MetricSkewNegotiated      = "mcp_skew_negotiated"
)

// skewAgg collects the version-skew scenario's custom metrics.
type skewAgg struct {
	reqs       float64
	replicas   map[string]float64            // requests per replica tag
	failures   map[string]map[string]float64 // kind -> reason -> count
	failMs     map[string][]float64          // kind -> time until the failure surfaced
	negotiated map[string]map[string]float64 // protocol -> replica -> successful connects
}

func addTo(m map[string]map[string]float64, k1, k2 string, v float64) map[string]map[string]float64 {
	if m == nil {
		m = map[string]map[string]float64{}
	}
	if m[k1] == nil {
		m[k1] = map[string]float64{}
	}
	m[k1][k2] += v
	return m
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func (s *skewAgg) add(metric string, v float64, tags map[string]string) {
	switch metric {
	case MetricSkewRequests:
		s.reqs += v
		if r := tags["replica"]; r != "" {
			if s.replicas == nil {
				s.replicas = map[string]float64{}
			}
			s.replicas[r] += v
		}
	case MetricSkewFailures:
		s.failures = addTo(s.failures, orUnknown(tags["kind"]), orUnknown(tags["reason"]), v)
	case MetricSkewFailureDuration:
		if s.failMs == nil {
			s.failMs = map[string][]float64{}
		}
		k := orUnknown(tags["kind"])
		s.failMs[k] = append(s.failMs[k], v)
	case MetricSkewNegotiated:
		s.negotiated = addTo(s.negotiated, orUnknown(tags["protocol"]), orUnknown(tags["replica"]), v)
	}
}

// SkewStats summarises a version-skew run. Failures maps kind (fast, slow,
// hang) to reason to count; FailureMs is the time until failures of each
// kind surfaced (ms); Replicas counts requests by the replica that answered
// (where known); Negotiated counts successful connects by protocol and the
// replica that answered the handshake.
type SkewStats struct {
	Requests   int64
	Failures   map[string]map[string]int64
	FailureMs  map[string]Latency
	Replicas   map[string]int64
	Negotiated map[string]map[string]int64
}

func roundMap(m map[string]map[string]float64) map[string]map[string]int64 {
	out := make(map[string]map[string]int64, len(m))
	for k1, inner := range m {
		out[k1] = make(map[string]int64, len(inner))
		for k2, v := range inner {
			out[k1][k2] = round(v)
		}
	}
	return out
}

// Skew returns the version-skew metrics, or nil when the run emitted none
// (any scenario other than version-skew).
func (a *Aggregator) Skew() *SkewStats {
	s := a.sk
	if s.reqs == 0 && len(s.failures) == 0 {
		return nil
	}
	out := &SkewStats{Requests: round(s.reqs), Failures: roundMap(s.failures), FailureMs: map[string]Latency{},
		Replicas: map[string]int64{}, Negotiated: roundMap(s.negotiated)}
	for k, d := range s.failMs {
		out.FailureMs[k] = latencyOf(d)
	}
	for r, v := range s.replicas {
		out.Replicas[r] = round(v)
	}
	return out
}
