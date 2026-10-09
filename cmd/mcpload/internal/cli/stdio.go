package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/analysis"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/k6run"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/sampler"
)

// The stdio transport: instead of an MCP endpoint (--url), mcpload gets the
// command of a local MCP server (--command) and the engine starts one server
// process per session, speaking JSON-RPC over its stdin/stdout. The scenario
// gets the command as env:
//
//   - MCP_COMMAND      JSON array of strings: program and arguments (-e)
//   - MCP_COMMAND_CWD  working directory, when --command-cwd is set (-e)
//   - MCP_COMMAND_ENV  JSON object of the --command-env variables. It goes into
//     the k6 process environment, not on the k6 command line, where any local
//     user could read it (k6 run passes system env vars to the script).
//
// The engine records each live server process as a file named by its pid in
// MCPLOAD_PID_DIR, a temporary directory mcpload creates for the run; the
// process sampler reads it.

// Env keys of the stdio transport.
const (
	envCommand    = "MCP_COMMAND"
	envCommandEnv = "MCP_COMMAND_ENV"
	envCommandCwd = "MCP_COMMAND_CWD"
)

// lbCheckScenario is the options.tags.scenario_name of scenarios/lb-check.js.
const lbCheckScenario = "lb-check"

// httpOnlyScenarios test a load balancer in front of HTTP replicas, which a
// stdio server does not have.
var httpOnlyScenarios = []string{lbCheckScenario, versionSkewScenario}

// validateTransport checks --url/--command/--transport and the flags that
// only make sense with one of them, and sets o.stdio, o.argv and o.cmdEnv.
func (o *runOpts) validateTransport() error {
	switch o.transport {
	case "", "auto", report.TransportHTTP, report.TransportStdio:
	default:
		return fmt.Errorf("--transport must be auto, http or stdio (got %q)", o.transport)
	}
	if o.url != "" && o.command != "" {
		return errors.New("--url and --command are mutually exclusive: --url tests a server over HTTP, --command starts a local server and talks to it over stdio")
	}
	o.stdio = o.command != ""
	switch {
	case o.transport == report.TransportStdio && !o.stdio:
		return errors.New("--transport stdio needs --command (the server to start)")
	case o.transport == report.TransportHTTP && o.stdio:
		return errors.New("--transport http needs --url, not --command")
	case o.url == "" && !o.stdio:
		return errors.New("--url (an MCP endpoint) or --command (a local stdio server) is required")
	}
	if !o.stdio {
		if len(o.commandEnv) > 0 || o.commandCwd != "" {
			return errors.New("--command-env and --command-cwd need --command")
		}
		for _, kv := range o.env {
			if k, _, _ := strings.Cut(kv, "="); k == envCommand || k == envCommandEnv || k == envCommandCwd || k == sampler.PIDDirEnv {
				return fmt.Errorf("--env %s: use --command, --command-env and --command-cwd", k)
			}
		}
		return nil
	}

	argv, err := splitCommand(o.command)
	if err != nil {
		return fmt.Errorf("--command: %v", err)
	}
	o.argv = argv
	if o.cmdEnv, err = parseCommandEnv(o.commandEnv); err != nil {
		return err
	}
	if o.commandCwd != "" {
		abs, err := filepath.Abs(o.commandCwd)
		if err == nil {
			var st os.FileInfo
			if st, err = os.Stat(abs); err == nil && !st.IsDir() {
				err = errors.New("not a directory")
			}
		}
		if err != nil {
			return fmt.Errorf("--command-cwd %s: %v", o.commandCwd, err)
		}
		o.commandCwd = abs
	}
	for _, kv := range o.env {
		switch k, _, _ := strings.Cut(kv, "="); k {
		case "MCP_URL", envCommand, envCommandEnv, envCommandCwd, sampler.PIDDirEnv:
			return fmt.Errorf("--env %s: with --command, use --command, --command-env and --command-cwd", k)
		}
	}
	name := strings.TrimSuffix(filepath.Base(o.scenario), ".js")
	if slices.Contains(httpOnlyScenarios, name) {
		return fmt.Errorf("the %s scenario tests HTTP replicas behind a load balancer; it does not apply to a stdio server (--command)", name)
	}
	if o.chaosRestart != 0 || o.chaosContainer != "" {
		return errors.New("--chaos-restart restarts a Docker container; it does not apply to a stdio server (--command)")
	}
	if o.callsURL != "" {
		return errors.New("--calls-url reads the executed call ids from an HTTP endpoint of the server; it does not apply to a stdio server (--command)")
	}
	return nil
}

// stdioEnv returns the -e values of a stdio run: MCP_COMMAND, MCP_COMMAND_CWD
// and an empty MCP_URL (so an MCP_URL in the OS environment does not reach
// the scenario).
func (o *runOpts) stdioEnv() [][2]string {
	argv, _ := json.Marshal(o.argv)
	out := [][2]string{{"MCP_URL", ""}, {envCommand, string(argv)}}
	if o.commandCwd != "" {
		out = append(out, [2]string{envCommandCwd, o.commandCwd})
	}
	return out
}

// stdioOSEnv returns what a stdio run adds to the k6 process environment:
// MCP_COMMAND_ENV (always, "{}" without --command-env, so a value in mcpload's
// own environment does not leak in) and MCPLOAD_PID_DIR.
func (o *runOpts) stdioOSEnv(pidDir string) []string {
	m := o.cmdEnv
	if m == nil {
		m = map[string]string{}
	}
	js, _ := json.Marshal(m)
	return []string{envCommandEnv + "=" + string(js), sampler.PIDDirEnv + "=" + pidDir}
}

// targetOf is report.run.target for the run.
func (o *runOpts) targetOf() report.Target {
	if o.stdio {
		return report.Target{Command: redactArgs(o.argv), Transport: report.TransportStdio, Label: o.label}
	}
	return report.Target{URL: redactURL(o.url), Transport: report.TransportHTTP, Label: o.label}
}

// commandLookupWarning returns a warning when the program of --command is a
// bare name that is not on PATH (the engine would fail to start every
// session). Paths are not checked: a relative one resolves against
// --command-cwd in the engine.
func (o *runOpts) commandLookupWarning() string {
	prog := o.argv[0]
	if strings.ContainsAny(prog, `/\`) {
		return ""
	}
	if _, ok := o.cmdEnv["PATH"]; ok {
		return ""
	}
	if _, err := exec.LookPath(prog); err != nil && !errors.Is(err, exec.ErrDot) {
		return fmt.Sprintf("warning: --command: %q was not found on PATH; the server processes will fail to start", prog)
	}
	return ""
}

// processInput converts the engine's process metrics for the stdio verdicts.
func processInput(p *k6run.ProcessStats) analysis.ProcessInput {
	if p == nil {
		return analysis.ProcessInput{}
	}
	in := analysis.ProcessInput{Present: true, Spawned: p.Spawns.Count, SpawnP95Ms: p.Spawns.P95,
		InvalidLines: p.InvalidLines, Exits: p.Exits, Unexpected: p.Unexpected}
	for _, c := range p.ExitCodes {
		in.ExitCodes = append(in.ExitCodes, analysis.ExitCode{Code: c.Code, Count: c.Count})
	}
	return in
}
