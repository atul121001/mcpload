package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

// fakeDocker records the commands it is asked to run; docker is never called.
type fakeDocker struct {
	mu    sync.Mutex
	calls [][]string
	err   error
	delay time.Duration
}

func (f *fakeDocker) run(_ context.Context, name string, args ...string) ([]byte, error) {
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.err != nil {
		return []byte("Error response from daemon: No such container: x"), f.err
	}
	return []byte("x\n"), nil
}

func TestScheduleRestart(t *testing.T) {
	logf := func(string, ...any) {}
	cases := []struct {
		name     string
		after    time.Duration
		wait     time.Duration // before stop
		err      error
		wantRan  bool
		wantErr  bool
		wantCall bool
	}{
		{name: "restarts the named container once it is due", after: 10 * time.Millisecond, wait: 60 * time.Millisecond, wantRan: true, wantCall: true},
		{name: "run ends first: nothing is restarted", after: time.Hour, wait: 5 * time.Millisecond},
		{name: "docker fails", after: 5 * time.Millisecond, wait: 60 * time.Millisecond, err: errors.New("exit status 1"), wantRan: true, wantErr: true, wantCall: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeDocker{err: c.err}
			start := time.Now()
			stop := scheduleRestart(c.after, "mcpload-chaos-ts", f.run, logf)
			time.Sleep(c.wait)
			res := stop()
			if res.Ran != c.wantRan || (res.Err != nil) != c.wantErr {
				t.Fatalf("got %+v", res)
			}
			if c.wantErr && !strings.Contains(res.Err.Error(), "No such container") {
				t.Errorf("error should carry docker's output: %v", res.Err)
			}
			if c.wantRan && res.Start.Sub(start) < c.after {
				t.Errorf("restart at %v, before it was due (%v)", res.Start.Sub(start), c.after)
			}
			want := 0
			if c.wantCall {
				want = 1
			}
			if len(f.calls) != want {
				t.Fatalf("docker calls: %v", f.calls)
			}
			if want == 1 && strings.Join(f.calls[0], " ") != "docker restart mcpload-chaos-ts" {
				t.Errorf("ran %v", f.calls[0])
			}
			if again := stop(); again != res {
				t.Errorf("second stop: %+v", again)
			}
		})
	}

	// stop while docker restart is running waits for it to finish.
	f := &fakeDocker{delay: 50 * time.Millisecond}
	stop := scheduleRestart(time.Millisecond, "c", f.run, logf)
	time.Sleep(10 * time.Millisecond)
	if res := stop(); !res.Ran || res.Duration < 40*time.Millisecond {
		t.Errorf("stop during the restart: %+v", res)
	}
}

func TestChaosFlags(t *testing.T) {
	cases := []struct {
		name, err, container string
		args                 []string
	}{
		{name: "restart without a container is refused", args: []string{"--chaos-restart", "30s"}, err: "needs --chaos-container"},
		{name: "defaults to the sampler container", args: []string{"--chaos-restart", "30s", "--sampler", "docker", "--container", "srv"}, container: "srv"},
		{name: "explicit container wins", args: []string{"--chaos-restart", "30s", "--container", "srv", "--chaos-container", "other"}, container: "other"},
		{name: "container without a restart", args: []string{"--chaos-container", "srv"}, err: "needs --chaos-restart"},
		{name: "negative", args: []string{"--chaos-restart", "-1s", "--chaos-container", "srv"}, err: "must not be negative"},
		{name: "bad calls url", args: []string{"--calls-url", "localhost:3019/calls"}, err: "must be an http(s) URL"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := &runOpts{set: map[string]bool{}}
			fs := runFlags(o, io.Discard)
			args := append([]string{"--url", "http://localhost:3001/mcp", "--scenario", "chaos_test.go"}, c.args...)
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			err := o.validate(nil)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if o.chaosContainer != c.container {
				t.Errorf("chaos container %q, want %q", o.chaosContainer, c.container)
			}
		})
	}
}

func TestLongLivedPhases(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want report.Phases
	}{
		{map[string]string{}, report.Phases{WarmupEndS: 60, LoadEndS: 660, CooldownEndS: 780}},
		{map[string]string{"SESSION_MIN": "1", "WARMUP_MIN": "0.25", "COOLDOWN_MIN": "0.25"}, report.Phases{WarmupEndS: 15, LoadEndS: 75, CooldownEndS: 90}},
		{map[string]string{"WARMUP_MIN": "0", "COOLDOWN_MIN": "0", "SESSION_MIN": "2"}, report.Phases{WarmupEndS: 0, LoadEndS: 120, CooldownEndS: 120}},
	}
	for _, c := range cases {
		got, err := longLivedPhases(c.env)
		if err != nil || got != c.want {
			t.Errorf("%v: got %+v %v, want %+v", c.env, got, err, c.want)
		}
	}
	if _, err := longLivedPhases(map[string]string{"SESSION_MIN": "ten"}); err == nil {
		t.Error("want an error for a non-number")
	}
}

func TestRecoveryConfig(t *testing.T) {
	cfg, err := recoveryConfig(map[string]string{"ERR_RATE": "0.05", "RECOVERY_BUDGET": "10"})
	if err != nil || cfg.BudgetS != 10 || cfg.WindowS != 5 || cfg.ErrRate != 0.05 || cfg.ConnectP95Ms != 1500 {
		t.Errorf("got %+v %v", cfg, err)
	}
	if _, err := recoveryConfig(map[string]string{"RECOVERY_WINDOW": "0"}); err == nil {
		t.Error("want an error for a zero window")
	}
}

func TestFetchExecutions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("prefix") {
		case "ab12-":
			w.Write([]byte(`{"executions":{"ab12-1-1":1,"ab12-1-2":2,"zz-1-1":1}}`))
		case "off-":
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"call tracking is off (TRACK_CALLS=1)"}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	got, err := fetchExecutions(context.Background(), srv.Client(), srv.URL+"/calls", "ab12-")
	if err != nil || len(got) != 2 || got["ab12-1-2"] != 2 {
		t.Errorf("got %v %v (ids of other runs must be dropped)", got, err)
	}
	if _, err := fetchExecutions(context.Background(), srv.Client(), srv.URL+"/calls", "off-"); err == nil || !strings.Contains(err.Error(), "TRACK_CALLS") {
		t.Errorf("404: %v", err)
	}
	if _, err := fetchExecutions(context.Background(), srv.Client(), srv.URL+"/calls", "x-"); err == nil {
		t.Error("an answer without executions must be an error")
	}
}
