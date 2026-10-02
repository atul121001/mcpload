// Package cli implements the mcpload command line: run, render, validate,
// upload and version. It uses the stdlib flag package with one FlagSet per
// subcommand.
package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/report"
	"github.com/atul121001/mcpload/cmd/mcpload/internal/upload"
)

// Version is the mcpload version (set from main via -ldflags).
var Version = "0.1.0-dev"

// Exit codes.
const (
	ExitPass  = 0 // run passed / command succeeded
	ExitFail  = 1 // run failed (a verdict failed or a threshold failed)
	ExitError = 2 // usage or runtime error
)

// errUsage marks errors that should print the subcommand usage.
var errUsage = errors.New("usage")

const usageText = `mcpload - load and soak testing for remote MCP servers

Usage:
  mcpload run --scenario <file.js> --url <mcp url> [flags]
  mcpload render <report.json> <out.html>
  mcpload validate <report.json>
  mcpload upload --url <upload server base url> --key <api key> <report.json>
  mcpload version

Run 'mcpload <command> -h' for the flags of a command.
Exit codes: 0 passed, 1 failed (verdict or threshold), 2 usage/runtime error.
`

// Main runs the CLI and returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return ExitError
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "run":
		return runCmd(rest, stdout, stderr)
	case "render":
		return renderCmd(rest, stdout, stderr)
	case "validate":
		return validateCmd(rest, stdout, stderr)
	case "upload":
		return uploadCmd(rest, stdout, stderr)
	case "version", "--version", "-version":
		fmt.Fprintf(stdout, "mcpload %s\n", Version)
		return ExitPass
	case "help", "-h", "--help", "-help":
		fmt.Fprint(stdout, usageText)
		return ExitPass
	default:
		fmt.Fprintf(stderr, "mcpload: unknown command %q\n\n%s", cmd, usageText)
		return ExitError
	}
}

// parseInterspersed parses flags that may appear before or after positional
// arguments (stdlib flag stops at the first positional).
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if rest[0] == "--" {
			return append(pos, rest[1:]...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func newFlagSet(name, synopsis string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s\n", synopsis)
		hasFlags := false
		fs.VisitAll(func(*flag.Flag) { hasFlags = true })
		if hasFlags {
			fmt.Fprintln(stderr, "\nFlags:")
			fs.PrintDefaults()
		}
	}
	return fs
}

// flagExit maps a flag parse error to an exit code (-h is success).
func flagExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return ExitPass
	}
	return ExitError
}

func renderCmd(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("render", "mcpload render <report.json> <out.html>", stderr)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) != 2 {
		fs.Usage()
		return ExitError
	}
	r, err := report.ReadJSON(pos[0])
	if err != nil {
		fmt.Fprintf(stderr, "mcpload render: %v\n", err)
		return ExitError
	}
	if err := writeHTML(pos[1], r); err != nil {
		fmt.Fprintf(stderr, "mcpload render: %v\n", err)
		return ExitError
	}
	fmt.Fprintf(stdout, "wrote %s\n", pos[1])
	return ExitPass
}

func writeHTML(path string, r *report.Report) error {
	var buf bytes.Buffer
	if err := report.RenderHTML(r, &buf); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func validateCmd(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("validate", "mcpload validate <report.json>", stderr)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) != 1 {
		fs.Usage()
		return ExitError
	}
	r, err := report.ReadJSON(pos[0])
	if err != nil {
		fmt.Fprintf(stderr, "mcpload validate: %v\n", err)
		return ExitError
	}
	if err := r.Check(); err != nil {
		fmt.Fprintf(stderr, "mcpload validate: %s is invalid:\n", pos[0])
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintf(stderr, "  - %s\n", line)
		}
		return ExitError
	}
	result := "PASS"
	if !report.Passed(r) {
		result = "FAIL"
	} else if report.HasWarnings(r) {
		result = "PASS (with warnings)"
	}
	fmt.Fprintf(stdout, "%s: valid (schemaVersion %s, run %s, result %s)\n", pos[0], r.SchemaVersion, r.Run.ID, result)
	return ExitPass
}

func uploadCmd(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("upload", "mcpload upload --url <upload server base url> --key <api key> <report.json>", stderr)
	url := fs.String("url", "", "base URL of a server that accepts report uploads; the report is POSTed to {url}/api/v1/runs")
	key := fs.String("key", "", "API key for that server (default: $MCPLOAD_KEY)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) != 1 || *url == "" {
		fs.Usage()
		return ExitError
	}
	k := *key
	if k == "" {
		k = os.Getenv("MCPLOAD_KEY")
	}
	body, err := os.ReadFile(pos[0])
	if err != nil {
		fmt.Fprintf(stderr, "mcpload upload: %v\n", err)
		return ExitError
	}
	if err := doUpload(*url, k, body, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "mcpload upload: %v\n", err)
		return ExitError
	}
	return ExitPass
}

func doUpload(base, key string, body []byte, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := &upload.Client{UserAgent: "mcpload/" + Version}
	resp, raw, err := c.Upload(ctx, base, key, body)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s\n", bytes.TrimSpace(raw))
	if resp.URL != "" {
		fmt.Fprintf(stderr, "mcpload: uploaded run %s (status %s): %s\n", resp.RunID, resp.Status, resp.URL)
	}
	return nil
}
