package k6run

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestResilienceAggregation(t *testing.T) {
	pt := func(metric string, ms int, v float64, tags string) string {
		at := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC).Add(time.Duration(ms) * time.Millisecond).Format(time.RFC3339Nano)
		return fmt.Sprintf(`{"metric":%q,"type":"Point","data":{"time":%q,"value":%g,"tags":{%s}}}`, metric, at, v, tags)
	}
	lines := []string{
		pt("mcp_reqs", 0, 1, `"method":"tools/call","tool":"fast"`),
		pt("mcp_reqs", 50, 1, `"method":"tools/call","tool":"flaky"`),
		pt("mcp_errors", 50, 1, `"method":"tools/call","tool":"flaky","error_type":"tool_iserror"`), // not a server error
		pt("mcp_reqs", 250, 1, `"method":"initialize"`),
		pt("mcp_errors", 250, 1, `"method":"initialize","error_type":"http"`),
		pt("mcp_connect_duration", 250, 1, `"method":"initialize","error_type":"http"`),
		pt("mcp_connect_duration", 260, 12, `"method":"initialize"`),
		pt("mcp_req_duration", 300, 40, `"method":"tools/call","tool":"search","session_age":"early"`),
		pt("mcp_req_duration", 310, 90, `"method":"tools/call","tool":"search","session_age":"late"`),
		pt("mcp_req_duration", 320, 900, `"method":"tools/call","tool":"search","session_age":"late","error_type":"timeout"`),
		pt("mcp_session_lifetime", 400, 61000, `"outcome":"survived"`),
		pt("mcp_session_lifetime", 410, 47000, `"outcome":"died","cause":"session_not_found"`),
		pt("mcp_session_reconnects", 420, 1, ``),
		pt("mcp_reconnects", 1500, 1, ``),
		pt("mcp_session_breaks", 1200, 2, `"cause":"http"`),
		pt("mcp_calls_tagged", 100, 3, ``),
		pt("mcp_call_attempts", 200, 1, `"call_id":"p-1-2","outcome":"http"`),
		pt("mcp_call_attempts", 1600, 1, `"call_id":"p-1-2","outcome":"answered"`),
	}
	a := NewAggregator(time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC), 10*time.Second, nil)
	if err := a.Read(strings.NewReader(strings.Join(lines, "\n"))); err != nil {
		t.Fatal(err)
	}
	fine := a.Fine()
	if len(fine) != 3 {
		t.Fatalf("fine bins: %d, want 3 (100 ms each, up to the last request or connect)", len(fine))
	}
	if fine[0].Reqs != 2 || fine[0].Errors != 0 {
		t.Errorf("bin 0 = %+v: tool_iserror must not count as an error", fine[0])
	}
	if b := fine[2]; b.Reqs != 1 || b.Errors != 1 || b.ByType["http"] != 1 || b.Connects != 2 || b.ConnectFails != 1 || len(b.ConnectMs) != 1 {
		t.Errorf("bin 2 = %+v", b)
	}
	ends, reconnects, ok := a.LongSessions()
	if !ok || reconnects != 1 || len(ends) != 2 || !ends[1].Died || ends[1].Cause != "session_not_found" || ends[0].LifetimeMs != 61000 {
		t.Errorf("sessions: %+v %d %v", ends, reconnects, ok)
	}
	if e, l := a.SessionAgeTools("early"), a.SessionAgeTools("late"); e["search"].Calls != 1 || l["search"].Calls != 1 || l["search"].P95 != 90 {
		t.Errorf("session_age tools: early %+v late %+v (failed calls must be left out)", e, l)
	}
	at, breaks := a.Reconnects()
	if len(at) != 1 || at[0] != 1.5 || breaks["http"] != 2 {
		t.Errorf("reconnects %v breaks %v", at, breaks)
	}
	tagged, attempts := a.CallAttempts()
	if tagged != 3 || strings.Join(attempts["p-1-2"], ",") != "http,answered" {
		t.Errorf("calls: %d %v", tagged, attempts)
	}
}
