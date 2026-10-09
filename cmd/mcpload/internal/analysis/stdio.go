package analysis

import (
	"fmt"
	"strings"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
)

// ProcessInput is what the stdio verdicts need from the engine's process
// metrics. Present is false when the run emitted none of them (an engine
// without stdio support, or no process was ever started).
type ProcessInput struct {
	Present bool
	// Spawned is the number of processes started (mcp_process_spawn_duration
	// samples) and SpawnP95Ms their spawn time p95.
	Spawned    int64
	SpawnP95Ms float64
	// InvalidLines is mcp_stdout_invalid_lines.
	InvalidLines int64
	// Exits counts mcp_process_exits; Unexpected those tagged expected=false,
	// with ExitCodes their exit codes and counts, most frequent first.
	Exits, Unexpected int64
	ExitCodes         []ExitCode
}

// ExitCode is one exit code of unexpected process exits.
type ExitCode struct {
	Code  string
	Count int64
}

// StdoutPollutionVerdict judges mcp_stdout_invalid_lines: a stdio server
// must write only JSON-RPC messages to stdout, so any other line fails.
func StdoutPollutionVerdict(in ProcessInput) report.Verdict {
	const id, signal = report.VerdictStdoutPollution, "mcp_stdout_invalid_lines"
	if !in.Present {
		return skipped(id, signal, "the engine reported no stdio process metrics")
	}
	v := report.Verdict{ID: id, Signal: signal, Status: report.StatusPass,
		Message: "The server wrote only JSON-RPC messages to stdout."}
	if in.InvalidLines > 0 {
		v.Status = report.StatusFail
		v.Message = fmt.Sprintf("The server wrote %s to stdout that %s not JSON-RPC messages. Over stdio, stdout carries only the protocol: send logs and banners to stderr (a stray line can corrupt or stall the session).",
			plural(in.InvalidLines, "line", "lines"), pick(in.InvalidLines, "was", "were"))
	}
	return v
}

// ProcessExitVerdict judges mcp_process_exits{expected:"false"}: a server
// process that exited while its session was still in use fails.
func ProcessExitVerdict(in ProcessInput) report.Verdict {
	const id, signal = report.VerdictProcessExit, "mcp_process_exits{expected:false}"
	if !in.Present {
		return skipped(id, signal, "the engine reported no stdio process metrics")
	}
	v := report.Verdict{ID: id, Signal: signal, Status: report.StatusPass}
	if in.Unexpected > 0 {
		v.Status = report.StatusFail
		var codes []string
		for _, c := range in.ExitCodes {
			codes = append(codes, fmt.Sprintf("%s ×%d", c.Code, c.Count))
		}
		v.Message = plural(in.Unexpected, "server process", "server processes") + " exited unexpectedly (crashed, or exited while the session was in use)"
		if len(codes) > 0 {
			v.Message += "; exit codes: " + strings.Join(codes, ", ")
		}
		v.Message += ". Check the server's stderr."
		return v
	}
	v.Message = "No server process exited unexpectedly"
	var facts []string
	if in.Spawned > 0 {
		facts = append(facts, fmt.Sprintf("%d started, spawn p95 %.0f ms", in.Spawned, in.SpawnP95Ms))
	}
	if in.Exits > 0 {
		facts = append(facts, fmt.Sprintf("%d exited when their session closed", in.Exits))
	}
	if len(facts) > 0 {
		v.Message += " (" + strings.Join(facts, "; ") + ")"
	}
	v.Message += "."
	return v
}

// stdioGeneratorNote is added to the generator verdict of stdio runs.
const stdioGeneratorNote = "The server processes ran on the same machine as k6 (stdio) and share its CPU, so k6 CPU use and latency include that contention."

func pick(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
