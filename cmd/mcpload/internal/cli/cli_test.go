package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestSoakPhasesDefaults(t *testing.T) {
	p, err := soakPhases(map[string]string{})
	if err != nil || p.WarmupEndS != 180 || p.LoadEndS != 1980 || p.CooldownEndS != 2280 {
		t.Errorf("defaults: %+v %v", p, err)
	}
	p, _ = soakPhases(map[string]string{"SOAK_MIN": "4"}) // warm-up min 1
	if p.WarmupEndS != 60 || p.LoadEndS != 300 || p.CooldownEndS != 600 {
		t.Errorf("soak 4: %+v", p)
	}
	p, _ = soakPhases(map[string]string{"SOAK_MIN": "4", "WARMUP_MIN": "0", "COOLDOWN_MIN": "0.5"})
	if p.WarmupEndS != 0 || p.LoadEndS != 240 || p.CooldownEndS != 270 {
		t.Errorf("explicit: %+v", p)
	}
	if _, err := soakPhases(map[string]string{"SOAK_MIN": "x"}); err == nil {
		t.Error("want error")
	}
}

func TestK6EnvPrecedence(t *testing.T) {
	o := &runOpts{url: "http://u/mcp", protocol: "auto", soakMin: 4, set: map[string]bool{"soak-min": true},
		env: multiFlag{"SOAK_MIN=9", "RATE=5", "MCP_PROTOCOL=2026-07-28"}}
	env, m := o.k6Env()
	if m["SOAK_MIN"] != "4" || m["RATE"] != "5" || m["MCP_URL"] != "http://u/mcp" || m["MCP_PROTOCOL"] != "2026-07-28" {
		t.Errorf("env = %v", env)
	}
	if _, ok := m["WARMUP_MIN"]; ok {
		t.Error("unset flags must not be passed")
	}
}

func TestParseInterspersed(t *testing.T) {
	fs := newFlagSet("x", "x", &bytes.Buffer{})
	url := fs.String("url", "", "")
	pos, err := parseInterspersed(fs, []string{"a.json", "--url", "http://d"})
	if err != nil || *url != "http://d" || len(pos) != 1 || pos[0] != "a.json" {
		t.Errorf("pos=%v url=%q err=%v", pos, *url, err)
	}
}

func TestUsageErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if c := Main([]string{"run", "--url", "http://x"}, &out, &errb); c != ExitError {
		t.Errorf("missing scenario: %d", c)
	}
	if c := Main([]string{"bogus"}, &out, &errb); c != ExitError {
		t.Errorf("bogus: %d", c)
	}
	out.Reset()
	if c := Main([]string{"version"}, &out, &errb); c != ExitPass || !strings.HasPrefix(out.String(), "mcpload ") {
		t.Errorf("version: %d %q", c, out.String())
	}
	if c := Main([]string{"validate", "nonexistent.json"}, &out, &errb); c != ExitError {
		t.Errorf("validate missing: %d", c)
	}
}

func TestScriptEnvMergesOSEnv(t *testing.T) {
	o := &runOpts{url: "http://u/mcp", protocol: "auto", vus: 3, set: map[string]bool{"vus": true},
		env: multiFlag{"RATE=5"}}
	_, explicit := o.k6Env()
	osEnv := []string{"SOAK_MIN=2", "WARMUP_MIN=0", "RATE=50", "VUS=9", `=C:=C:\x`, "PATH=/bin"}
	m := scriptEnv(osEnv, explicit)
	// OS values reach the script unless --env or a flag overrides them.
	if m["SOAK_MIN"] != "2" || m["WARMUP_MIN"] != "0" || m["RATE"] != "5" || m["VUS"] != "3" || m["MCP_URL"] != "http://u/mcp" {
		t.Errorf("env = %v", m)
	}
	if _, ok := m[""]; ok {
		t.Error("empty key kept")
	}
	p, err := soakPhases(m)
	if err != nil || p.WarmupEndS != 0 || p.LoadEndS != 120 || p.CooldownEndS != 420 {
		t.Errorf("phases from OS env = %+v %v", p, err)
	}
}
