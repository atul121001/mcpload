package k6run

import (
	"os"
	"slices"
	"testing"
)

func TestEngineEnvTurnsOffUsageReport(t *testing.T) {
	in := []string{"PATH=/bin", "MCP_TOKEN=x"}
	got := EngineEnv(in)
	if want := []string{"PATH=/bin", "MCP_TOKEN=x", "K6_NO_USAGE_REPORT=true"}; !slices.Equal(got, want) {
		t.Fatalf("EngineEnv = %q, want %q", got, want)
	}
	if len(in) != 2 {
		t.Fatalf("input modified: %q", in)
	}
	// An empty value is not a choice: the added value wins (exec keeps the last).
	if got := EngineEnv([]string{"K6_NO_USAGE_REPORT="}); !slices.Equal(got, []string{"K6_NO_USAGE_REPORT=", "K6_NO_USAGE_REPORT=true"}) {
		t.Errorf("empty value: %q", got)
	}
	// An explicit setting, either way, is kept as is.
	for _, env := range [][]string{
		{"PATH=/bin", "K6_NO_USAGE_REPORT=false"},
		{"K6_NO_USAGE_REPORT=1"},
		{"k6_no_usage_report=false"},
	} {
		if got := EngineEnv(env); !slices.Equal(got, env) {
			t.Errorf("EngineEnv(%q) = %q, want it unchanged", env, got)
		}
	}
}

// `k6 run` is started with the OS environment and the usage report turned off.
func TestRunCommandEnv(t *testing.T) {
	t.Setenv("MCPLOAD_TEST_VAR", "kept") // also restores K6_NO_USAGE_REPORT below
	t.Setenv("K6_NO_USAGE_REPORT", "")
	os.Unsetenv("K6_NO_USAGE_REPORT")
	cmd := RunConfig{Bin: "k6", Script: "s.js", NDJSONPath: "m.ndjson", SummaryPath: "s.json"}.command()
	if !slices.Contains(cmd.Env, "K6_NO_USAGE_REPORT=true") || !slices.Contains(cmd.Env, "MCPLOAD_TEST_VAR=kept") {
		t.Fatalf("env %q", cmd.Env)
	}
}
