package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type composeCall struct {
	yaml, project string
	args          []string
}

// fakeDemo replaces docker and the health probe for the demo command.
func fakeDemo(t *testing.T, healthy func(url string) bool) *[]composeCall {
	t.Helper()
	var calls []composeCall
	oldCheck, oldCompose, oldHealthy, oldPoll := demoCheckDocker, demoCompose, demoHealthy, demoPoll
	t.Cleanup(func() {
		demoCheckDocker, demoCompose, demoHealthy, demoPoll = oldCheck, oldCompose, oldHealthy, oldPoll
	})
	demoCheckDocker = func() error { return nil }
	demoCompose = func(_ context.Context, yaml, project string, args []string, _, _ io.Writer) error {
		calls = append(calls, composeCall{yaml, project, args})
		return nil
	}
	demoHealthy = func(_ context.Context, url string) bool { return healthy(url) }
	demoPoll = time.Millisecond
	return &calls
}

func TestDemoUp(t *testing.T) {
	t.Setenv("MCPLOAD_DEMO_TAG", "")
	t.Setenv("MCPLOAD_DEMO_IMAGE_PREFIX", "")
	old := Version
	Version = "v0.4.0"
	t.Cleanup(func() { Version = old })
	var mu sync.Mutex
	var probed []string
	calls := fakeDemo(t, func(url string) bool { mu.Lock(); defer mu.Unlock(); probed = append(probed, url); return true })

	var out, errb bytes.Buffer
	if code := Main([]string{"demo", "up"}, &out, &errb); code != ExitPass {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errb.String())
	}
	if len(*calls) != 1 {
		t.Fatalf("compose calls: %+v", *calls)
	}
	c := (*calls)[0]
	if c.project != "mcpload-demo" || strings.Join(c.args, " ") != "up -d" {
		t.Errorf("compose -p %s %v", c.project, c.args)
	}
	if !strings.Contains(c.yaml, "image: ghcr.io/atul121001/mcpload-demo-ts:0.4.0") {
		t.Error("compose file does not pin the demo images to the CLI version")
	}
	if len(probed) != 11 {
		t.Errorf("probed %d /healthz URLs, want 11", len(probed))
	}
	for _, s := range []string{
		"http://localhost:3001/mcp", "ts-healthy", "http://localhost:3004/mcp", "session_not_found",
		"http://localhost:3011/mcp", "mcpload demo down",
	} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, out.String())
		}
	}
}

func TestDemoUpOverrides(t *testing.T) {
	t.Setenv("MCPLOAD_DEMO_TAG", "ci")
	t.Setenv("MCPLOAD_DEMO_IMAGE_PREFIX", "local/demo")
	calls := fakeDemo(t, func(string) bool { return true })
	var out, errb bytes.Buffer
	code := Main([]string{"demo", "up", "--project-name", "demo-test", "--base-port", "13001"}, &out, &errb)
	if code != ExitPass {
		t.Fatalf("exit %d\n%s", code, errb.String())
	}
	c := (*calls)[0]
	if c.project != "demo-test" || !strings.Contains(c.yaml, "image: local/demo-go:ci") || !strings.Contains(c.yaml, `"127.0.0.1:13011:3000"`) {
		t.Errorf("project %s, yaml:\n%s", c.project, c.yaml)
	}
	if !strings.Contains(out.String(), "http://localhost:13004/mcp") || !strings.Contains(out.String(), "mcpload demo down --project-name demo-test") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestDemoUpUnhealthy(t *testing.T) {
	fakeDemo(t, func(url string) bool { return !strings.Contains(url, ":3002/") })
	var out, errb bytes.Buffer
	if code := Main([]string{"demo", "up", "--timeout", "20ms"}, &out, &errb); code != ExitError {
		t.Fatalf("exit %d, want %d", code, ExitError)
	}
	if !strings.Contains(out.String(), "DOWN") || !strings.Contains(out.String(), "mcpload demo logs") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestDemoDownLogsStatus(t *testing.T) {
	calls := fakeDemo(t, func(string) bool { return false })
	var out, errb bytes.Buffer
	if code := Main([]string{"demo", "down"}, &out, &errb); code != ExitPass {
		t.Fatalf("down: exit %d", code)
	}
	if code := Main([]string{"demo", "logs", "-f", "--tail", "5", "ts-leaky"}, &out, &errb); code != ExitPass {
		t.Fatalf("logs: exit %d: %s", code, errb.String())
	}
	if code := Main([]string{"demo", "status"}, &out, &errb); code != ExitFail {
		t.Fatalf("status with nothing running: exit %d, want %d", code, ExitFail)
	}
	var got []string
	for _, c := range *calls {
		got = append(got, strings.Join(c.args, " "))
	}
	if want := "down|logs --follow --tail 5 ts-leaky|ps"; strings.Join(got, "|") != want {
		t.Errorf("compose args %q, want %q", strings.Join(got, "|"), want)
	}
}

func TestDemoNoDocker(t *testing.T) {
	fakeDemo(t, func(string) bool { return true })
	demoCheckDocker = func() error { return errNoDocker }
	var out, errb bytes.Buffer
	if code := Main([]string{"demo", "up"}, &out, &errb); code != ExitError {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errb.String(), "docker was not found") || !strings.Contains(errb.String(), "get-docker") {
		t.Errorf("stderr: %s", errb.String())
	}
}

func TestDemoConfigNeedsNoDocker(t *testing.T) {
	fakeDemo(t, func(string) bool { return true })
	demoCheckDocker = func() error { return errors.New("should not be called") }
	var out, errb bytes.Buffer
	if code := Main([]string{"demo", "config"}, &out, &errb); code != ExitPass {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "services:") {
		t.Errorf("config output:\n%s", out.String())
	}
}

func TestDemoUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"demo"}, &out, &errb); code != ExitError || !strings.Contains(errb.String(), "mcpload demo up") {
		t.Errorf("no subcommand: exit %d, stderr %q", code, errb.String())
	}
	if code := Main([]string{"demo", "start"}, &out, &errb); code != ExitError {
		t.Errorf("unknown subcommand: exit %d", code)
	}
	if code := Main([]string{"demo", "up", "extra"}, &out, &errb); code != ExitError {
		t.Errorf("positional arg: exit %d", code)
	}
}
