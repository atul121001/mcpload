package analysis

import (
	"strings"
	"testing"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

// bins builds 100 ms bins from per-second rows: each row is spread over its
// ten bins (reqs and errors per bin), with optional connects in the row's first bin.
type sec struct {
	reqs, errs   int64
	connects     []float64 // successful connect durations (ms)
	connectFails int64
}

func binsOf(rows []sec) []FineBin {
	var out []FineBin
	for _, r := range rows {
		for k := 0; k < 10; k++ {
			b := FineBin{Reqs: r.reqs, Errors: r.errs}
			if r.errs > 0 {
				b.ByType = map[string]int64{"http": r.errs}
			}
			if k == 0 {
				b.ConnectMs = r.connects
				b.Connects = int64(len(r.connects)) + r.connectFails
				b.ConnectFails = r.connectFails
			}
			out = append(out, b)
		}
	}
	return out
}

func repeat(n int, s sec) []sec {
	out := make([]sec, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func TestComputeRecovery(t *testing.T) {
	cfg := RecoveryConfig{BudgetS: 30, WindowS: 5, ErrRate: 0.01, ConnectP95Ms: 1500}
	ok := sec{reqs: 10}
	down := sec{reqs: 2, errs: 2, connectFails: 2}
	cases := []struct {
		name          string
		rows          []sec
		atS           float64
		reconnects    []float64
		wantRecovered bool
		wantS         float64
		wantBack      *float64
		wantFails     int64
	}{
		{name: "never down", rows: repeat(20, ok), atS: 5, wantRecovered: true, wantS: 0},
		{
			name: "3 s outage, then connects come back",
			rows: append(append(append(repeat(5, ok), repeat(3, down)...), sec{reqs: 10, connects: []float64{20, 30}}), repeat(10, ok)...),
			atS:  5, reconnects: []float64{8.1, 8.4, 2}, wantRecovered: true, wantS: 3, wantBack: report.F(3), wantFails: 6,
		},
		{
			name: "slow connects after the outage delay recovery",
			rows: append(append(append(repeat(5, ok), repeat(2, down)...), repeat(3, sec{reqs: 10, connects: []float64{2000}})...), repeat(10, ok)...),
			atS:  5, wantRecovered: true, wantS: 4.1, wantBack: report.F(2), wantFails: 4, // just after the last slow connect
		},
		{name: "never recovers before the end", rows: append(repeat(5, ok), repeat(10, down)...), atS: 5, wantRecovered: false, wantFails: 20},
		{name: "no traffic after the restart is not recovered", rows: append(repeat(5, ok), repeat(10, sec{})...), atS: 5, wantRecovered: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rc := ComputeRecovery(c.atS, binsOf(c.rows), c.reconnects, cfg)
			if rc.Recovered != c.wantRecovered {
				t.Fatalf("recovered = %v, want %v (%+v)", rc.Recovered, c.wantRecovered, rc)
			}
			if c.wantRecovered && (rc.RecoveryS == nil || *rc.RecoveryS != c.wantS) {
				t.Errorf("recoveryS = %v, want %v", rc.RecoveryS, c.wantS)
			}
			if !c.wantRecovered && rc.RecoveryS != nil {
				t.Errorf("recoveryS = %v, want nil", *rc.RecoveryS)
			}
			if (c.wantBack == nil) != (rc.ServerBackS == nil) || (c.wantBack != nil && *c.wantBack != *rc.ServerBackS) {
				t.Errorf("serverBackS = %v, want %v", rc.ServerBackS, c.wantBack)
			}
			if rc.ConnectFailures != c.wantFails {
				t.Errorf("connectFailures = %d, want %d", rc.ConnectFailures, c.wantFails)
			}
		})
	}
	rc := ComputeRecovery(5, binsOf(append(append(repeat(5, ok), repeat(3, down)...), repeat(10, sec{reqs: 10, connects: []float64{10}})...)), []float64{2, 8.1, 8.4}, cfg)
	if rc.Reconnects != 2 || rc.LastReconnectS == nil || *rc.LastReconnectS != 3.4 {
		t.Errorf("reconnects after the restart: %d, last %v; want 2, 3.4", rc.Reconnects, rc.LastReconnectS)
	}
	if rc.ErrorsByType["http"] != 60 {
		t.Errorf("errorsByType = %v, want http 60", rc.ErrorsByType)
	}
}

func TestRecoveryVerdict(t *testing.T) {
	rec := func(s *float64) *report.Recovery {
		return &report.Recovery{Recovered: s != nil, RecoveryS: s, ServerBackS: report.F(1.2), LastReconnectS: report.F(1.5), BudgetS: 30, WindowS: 5,
			ErrorRate: 0.01, ConnectP95Ms: 1500, Reconnects: 20, ConnectAttempts: 60, ConnectFailures: 40, ErrorsByType: map[string]int64{"http": 40, "session_not_found": 5}}
	}
	cases := []struct {
		name   string
		ch     *report.Chaos
		status string
		msg    string
	}{
		{"no chaos", nil, report.StatusSkipped, "no restart was injected"},
		{"not due", &report.Chaos{Action: "restart", Container: "c"}, report.StatusSkipped, "ended before"},
		{"docker failed", &report.Chaos{Action: "restart", Container: "c", Ran: true, AtS: 30, Error: "no such container"}, report.StatusWarn, "no such container"},
		{"within budget", &report.Chaos{Action: "restart", Container: "c", Ran: true, AtS: 30, DurationS: 0.8, Recovery: rec(report.F(4.8))}, report.StatusPass,
			"After the restart of c at 30 s (docker restart took 0.8 s), the server accepted sessions again after 1.2 s and 20 agents reconnected within 1.5 s (60 connect attempts, 40 failed); errors returned to under 1% with connect p95 under 1500 ms 4.8 s after the restart (budget 30 s). Errors during the outage: http 40, session_not_found 5."},
		{"over budget", &report.Chaos{Action: "restart", Container: "c", Ran: true, AtS: 30, Recovery: rec(report.F(45))}, report.StatusFail, "too slow"},
		{"never", &report.Chaos{Action: "restart", Container: "c", Ran: true, AtS: 30, Recovery: rec(nil)}, report.StatusFail, "never stayed under 1%"},
	}
	for _, c := range cases {
		v := RecoveryVerdict(c.ch)
		if v.ID != report.VerdictRecovery || v.Status != c.status || !strings.Contains(v.Message, c.msg) {
			t.Errorf("%s: got %s %q, want %s containing %q", c.name, v.Status, v.Message, c.status, c.msg)
		}
	}
}

func TestSessionSurvival(t *testing.T) {
	n := func(k int, s SessionEnd) []SessionEnd {
		out := make([]SessionEnd, k)
		for i := range out {
			out[i] = s
		}
		return out
	}
	alive := SessionEnd{LifetimeS: 600}
	dead := SessionEnd{LifetimeS: 302, Died: true, Cause: "session_not_found"}
	flat := map[string]PhaseP95{"search": {Calls: 100, P95: 40}}
	slower := map[string]PhaseP95{"search": {Calls: 100, P95: 120}}
	few := map[string]PhaseP95{"search": {Calls: 5, P95: 400}}
	cases := []struct {
		name        string
		ends        []SessionEnd
		early, late map[string]PhaseP95
		status, msg string
	}{
		{"none", nil, nil, nil, report.StatusSkipped, "no long-lived session"},
		{"all survive", n(50, alive), flat, flat, report.StatusPass, "All 50 sessions stayed open for their planned lifetime (median 10m00s). Late calls kept their latency; largest change: search p95 40.0 ms early"},
		{"deaths", append(n(32, alive), n(18, dead)...), flat, flat, report.StatusFail, "18 of 50 sessions died after a median 5m02s with session_not_found 18: the server dropped live sessions"},
		{"late calls slower", n(10, alive), flat, slower, report.StatusWarn, "calls got slower as sessions aged: search p95 40.0 ms early in the sessions → 120.0 ms late (×3.00)"},
		{"too few calls to compare", n(10, alive), flat, few, report.StatusPass, "Too few calls"},
	}
	for _, c := range cases {
		s := LongSessions(c.ends, 18, c.early, c.late)
		v := SessionSurvivalVerdict(s)
		if v.ID != report.VerdictSessionSurvival || v.Status != c.status || !strings.Contains(v.Message, c.msg) {
			t.Errorf("%s: got %s %q, want %s containing %q", c.name, v.Status, v.Message, c.status, c.msg)
		}
	}
	s := LongSessions(append(n(3, alive), dead, SessionEnd{LifetimeS: 100, Died: true, Cause: "http"}), 2, nil, nil)
	if s.Total != 5 || s.Survived != 3 || s.Died != 2 || s.DiedByCause["http"] != 1 || *s.DiedAfterS != 201 || s.LifetimeP50S != 600 {
		t.Errorf("summary: %+v", s)
	}
}

func TestCallIntegrity(t *testing.T) {
	cases := []struct {
		name        string
		in          IntegrityInput
		want        report.CallIntegrity
		status, msg string
	}{
		{
			name: "clean",
			in:   IntegrityInput{Source: "u", Tagged: 3, Executions: map[string]int64{"p-1-1": 1, "p-1-2": 1, "p-1-3": 1}},
			want: report.CallIntegrity{Source: "u", Tagged: 3, Executed: 3, Executions: 3}, status: report.StatusPass, msg: "No call ran twice",
		},
		{
			name: "failed on the client, one ran anyway",
			in: IntegrityInput{Source: "u", Tagged: 3, Attempts: map[string][]string{"p-1-2": {"http"}, "p-1-3": {"http"}},
				Executions: map[string]int64{"p-1-1": 1, "p-1-2": 1}},
			want:   report.CallIntegrity{Source: "u", Tagged: 3, ClientFailed: 2, Executed: 2, Executions: 2, FailedButExecuted: 1, NeverRan: 1},
			status: report.StatusWarn, msg: "1 calls failed on the client but ran on the server",
		},
		{
			name: "retry ran a call twice; a server-side double run too",
			in: IntegrityInput{Source: "u", Tagged: 3, Attempts: map[string][]string{"p-1-1": {"http", "answered"}, "p-1-2": {"session_not_found", "answered"}},
				Executions: map[string]int64{"p-1-1": 2, "p-1-2": 1, "p-1-3": 3}},
			want:   report.CallIntegrity{Source: "u", Tagged: 3, Retried: 2, Executed: 3, Executions: 6, Duplicated: 2, DuplicateExecutions: 3, DuplicatedAfterRetry: 1},
			status: report.StatusFail, msg: "2 calls ran more than once on the server (3 extra executions; 1 of them were re-sent by the client with the same call id, 1 were not)",
		},
	}
	for _, c := range cases {
		ci := CallIntegrity(c.in)
		if *ci != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, *ci, c.want)
		}
		v := CallIntegrityVerdict(c.in.Tagged, ci)
		if v.Status != c.status || !strings.Contains(v.Message, c.msg) {
			t.Errorf("%s: verdict %s %q, want %s containing %q", c.name, v.Status, v.Message, c.status, c.msg)
		}
	}
	if v := CallIntegrityVerdict(10, nil); v.Status != report.StatusSkipped || !strings.Contains(v.Message, "server-side executions not measured") {
		t.Errorf("no source: %+v", v)
	}
	if v := CallIntegrityVerdict(0, nil); v.Status != report.StatusSkipped || !strings.Contains(v.Message, "no tools/call carried a call id") {
		t.Errorf("no ids: %+v", v)
	}
}
