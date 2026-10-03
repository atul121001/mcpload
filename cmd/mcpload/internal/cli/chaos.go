package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/analysis"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/k6run"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

// Scenario names (scenario_name tags) of scenarios/long-lived.js and scenarios/reconnect-storm.js.
const (
	longLivedScenario      = "long-lived"
	reconnectStormScenario = "reconnect-storm"
)

// commandRunner runs an external command and returns its combined output.
// The chaos restart uses it to call the docker CLI; tests use a fake.
type commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// restartTimeout bounds one `docker restart` (docker waits up to 10 s for the
// container to stop before it kills it).
const restartTimeout = 2 * time.Minute

// chaosRun is the outcome of a scheduled restart. Ran is false when the
// schedule was stopped before the restart was due.
type chaosRun struct {
	Ran      bool
	Start    time.Time
	Duration time.Duration
	Err      error
}

// scheduleRestart runs `docker restart <container>` once, after `after`,
// unless stop is called first. stop cancels a restart that is not due yet,
// waits for one that is running, and returns the outcome. Only the container
// named here is ever touched.
func scheduleRestart(after time.Duration, container string, run commandRunner, logf func(string, ...any)) (stop func() chaosRun) {
	cancel := make(chan struct{})
	done := make(chan chaosRun, 1)
	go func() {
		t := time.NewTimer(after)
		defer t.Stop()
		select {
		case <-cancel:
			done <- chaosRun{}
			return
		case <-t.C:
		}
		logf("chaos: restarting container %s now (docker restart %s)", container, container)
		start := time.Now()
		ctx, c := context.WithTimeout(context.Background(), restartTimeout)
		out, err := run(ctx, "docker", "restart", container)
		c()
		res := chaosRun{Ran: true, Start: start, Duration: time.Since(start)}
		if err != nil {
			res.Err = fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
			logf("error: chaos: docker restart %s failed: %v", container, res.Err)
		} else {
			logf("chaos: %s restarted (docker restart took %.1f s)", container, res.Duration.Seconds())
		}
		done <- res
	}()
	var stopped chaosRun
	var once bool
	return func() chaosRun {
		if !once {
			once = true
			close(cancel)
			stopped = <-done
		}
		return stopped
	}
}

// envFloat reads a number from the script env, def when unset.
func envFloat(env map[string]string, k string, def float64) (float64, error) {
	v := strings.TrimSpace(env[k])
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%s must be a number, got %q", k, v)
	}
	return f, nil
}

// longLivedPhases mirrors scenarios/long-lived.js: WARMUP_MIN (1),
// SESSION_MIN (10), COOLDOWN_MIN (2); each rounded to whole seconds.
func longLivedPhases(env map[string]string) (report.Phases, error) {
	var vals [3]float64
	for i, k := range []struct {
		name string
		def  float64
	}{{"WARMUP_MIN", 1}, {"SESSION_MIN", 10}, {"COOLDOWN_MIN", 2}} {
		f, err := envFloat(env, k.name, k.def)
		if err != nil {
			return report.Phases{}, err
		}
		vals[i] = math.Max(0, math.Round(f*60))
	}
	w, l, c := vals[0], vals[1], vals[2]
	return report.Phases{WarmupEndS: w, LoadEndS: w + l, CooldownEndS: w + l + c}, nil
}

// recoveryConfig reads RECOVERY_BUDGET (30 s), RECOVERY_WINDOW (5 s),
// ERR_RATE (0.01) and CONNECT_P95_MS (1500) from the script env.
func recoveryConfig(env map[string]string) (analysis.RecoveryConfig, error) {
	var cfg analysis.RecoveryConfig
	for _, p := range []struct {
		k   string
		def float64
		dst *float64
	}{{"RECOVERY_BUDGET", 30, &cfg.BudgetS}, {"RECOVERY_WINDOW", 5, &cfg.WindowS}, {"ERR_RATE", 0.01, &cfg.ErrRate}, {"CONNECT_P95_MS", 1500, &cfg.ConnectP95Ms}} {
		f, err := envFloat(env, p.k, p.def)
		if err != nil {
			return cfg, err
		}
		if f <= 0 {
			return cfg, fmt.Errorf("%s must be > 0, got %v", p.k, f)
		}
		*p.dst = f
	}
	return cfg, nil
}

// chaosReport builds report.chaos from the restart outcome; atS is the
// restart start relative to the run origin.
func chaosReport(container string, cr chaosRun, origin time.Time, agg *k6run.Aggregator, cfg analysis.RecoveryConfig) *report.Chaos {
	ch := &report.Chaos{Action: "restart", Container: container, Ran: cr.Ran}
	if !cr.Ran {
		return ch
	}
	ch.AtS = round3(math.Max(0, cr.Start.Sub(origin).Seconds()))
	ch.DurationS = round3(cr.Duration.Seconds())
	if cr.Err != nil {
		ch.Error = cr.Err.Error()
		return ch
	}
	fine := agg.Fine()
	bins := make([]analysis.FineBin, len(fine))
	for i, f := range fine {
		bins[i] = analysis.FineBin{Reqs: f.Reqs, Errors: f.Errors, ByType: f.ByType, Connects: f.Connects, ConnectFails: f.ConnectFails, ConnectMs: f.ConnectMs}
	}
	reconnects, _ := agg.Reconnects()
	rc := analysis.ComputeRecovery(ch.AtS, bins, reconnects, cfg)
	ch.Recovery = &rc
	return ch
}

// longSessions builds report.sessions from the long-lived scenario's metrics (nil when absent).
func longSessions(agg *k6run.Aggregator) *report.Sessions {
	ends, reconnects, ok := agg.LongSessions()
	if !ok {
		return nil
	}
	in := make([]analysis.SessionEnd, len(ends))
	for i, e := range ends {
		in[i] = analysis.SessionEnd{LifetimeS: e.LifetimeMs / 1000, Died: e.Died, Cause: e.Cause}
	}
	return analysis.LongSessions(in, reconnects, phaseP95(agg.SessionAgeTools("early")), phaseP95(agg.SessionAgeTools("late")))
}

// maxCallsBody bounds the GET /calls answer (a few MB is millions of ids).
const maxCallsBody = 64 << 20

// fetchExecutions reads a server's record of executed call ids: GET
// <callsURL>?prefix=<prefix> answering {"executions": {"<call id>": count}}.
func fetchExecutions(ctx context.Context, hc *http.Client, callsURL, prefix string) (map[string]int64, error) {
	u, err := url.Parse(callsURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("prefix", prefix)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCallsBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("GET %s: HTTP %d: %s", u, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Executions map[string]int64 `json:"executions"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("GET %s: want {\"executions\": {...}}: %v", u, err)
	}
	if out.Executions == nil {
		return nil, fmt.Errorf("GET %s: no \"executions\" object in the answer", u)
	}
	// Ids that don't carry the prefix belong to another run.
	for id := range out.Executions {
		if !strings.HasPrefix(id, prefix) {
			delete(out.Executions, id)
		}
	}
	return out.Executions, nil
}
