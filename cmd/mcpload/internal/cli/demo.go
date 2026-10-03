package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/atul121001/mcpload/cmd/mcpload/internal/demo"
)

const demoUsage = `Usage:
  mcpload demo up       start the demo MCP servers and wait until they answer
  mcpload demo status   show which demo servers are running
  mcpload demo logs     show server logs (mcpload demo logs [-f] [service...])
  mcpload demo down     stop and remove the demo servers
  mcpload demo config   print the compose file mcpload runs

The demo servers are healthy and deliberately broken MCP servers on
localhost:3001-3011 (127.0.0.1 only). They run in Docker from the published
images, so you need Docker with Compose v2, nothing else.

Flags (all subcommands):
  --project-name <name>  compose project name (default mcpload-demo)
  --base-port <port>     host port of the first server; the others follow (default 3001)

Image overrides (env): MCPLOAD_DEMO_IMAGE_PREFIX (default ghcr.io/atul121001/mcpload-demo),
MCPLOAD_DEMO_TAG (default: this mcpload's version, or latest for development builds).
`

// Hooks for tests.
var (
	// demoCheckDocker reports whether docker and docker compose are usable.
	demoCheckDocker = checkDockerCompose
	// demoCompose runs `docker compose -p <project> -f - <args>` with the
	// compose file on stdin.
	demoCompose = runDockerCompose
	// demoHealthy reports whether url answers 2xx.
	demoHealthy = httpHealthy
	// demoPoll is the interval between /healthz probes in `demo up`.
	demoPoll = time.Second
)

type demoOpts struct {
	project  string
	basePort int
	timeout  time.Duration
	follow   bool
	tail     string
}

func demoCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		w := stderr
		if len(args) > 0 {
			w = stdout
		}
		fmt.Fprint(w, demoUsage)
		if len(args) == 0 {
			return ExitError
		}
		return ExitPass
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "up", "down", "status", "logs", "config":
	default:
		fmt.Fprintf(stderr, "mcpload demo: unknown command %q\n\n%s", sub, demoUsage)
		return ExitError
	}

	var o demoOpts
	fs := newFlagSet("demo "+sub, "mcpload demo "+sub+" [flags]", stderr)
	fs.StringVar(&o.project, "project-name", demo.DefaultProject, "docker compose project name; containers are named <project>-<service>-1")
	fs.IntVar(&o.basePort, "base-port", demo.DefaultBasePort, "host port of the first demo server; the others use the next 10 ports")
	if sub == "up" {
		fs.DurationVar(&o.timeout, "timeout", 3*time.Minute, "how long to wait for every server to answer /healthz")
	}
	if sub == "logs" {
		fs.BoolVar(&o.follow, "f", false, "follow the log output")
		fs.BoolVar(&o.follow, "follow", false, "follow the log output")
		fs.StringVar(&o.tail, "tail", "", "number of lines to show from the end of each log (default all)")
	}
	pos, err := parseInterspersed(fs, rest)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) > 0 && sub != "logs" {
		fs.Usage()
		return ExitError
	}

	yaml, err := demo.Compose(demo.Options{
		ImagePrefix: os.Getenv("MCPLOAD_DEMO_IMAGE_PREFIX"),
		Tag:         demoTag(),
		BasePort:    o.basePort,
	})
	if err != nil {
		fmt.Fprintf(stderr, "mcpload demo: %v\n", err)
		return ExitError
	}
	if sub == "config" {
		fmt.Fprint(stdout, yaml)
		return ExitPass
	}
	if err := demoCheckDocker(); err != nil {
		fmt.Fprintf(stderr, "mcpload demo: %v\n", err)
		return ExitError
	}

	ctx := context.Background()
	switch sub {
	case "up":
		return demoUp(ctx, yaml, o, stdout, stderr)
	case "down":
		if err := demoCompose(ctx, yaml, o.project, []string{"down"}, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "mcpload demo down: %v\n", err)
			return ExitError
		}
		return ExitPass
	case "status":
		if err := demoCompose(ctx, yaml, o.project, []string{"ps"}, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "mcpload demo status: %v\n", err)
			return ExitError
		}
		healthy := probeAll(ctx, o.basePort)
		fmt.Fprintln(stdout)
		printTargets(stdout, o.basePort, healthy, true)
		for _, ok := range healthy {
			if !ok {
				fmt.Fprintf(stdout, "\nNot every demo server is answering. Start them with: mcpload demo up%s\n", demoFlagsHint(o, true))
				return ExitFail
			}
		}
		return ExitPass
	default: // logs
		cargs := []string{"logs"}
		if o.follow {
			cargs = append(cargs, "--follow")
		}
		if o.tail != "" {
			cargs = append(cargs, "--tail", o.tail)
		}
		cargs = append(cargs, pos...)
		if err := demoCompose(ctx, yaml, o.project, cargs, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "mcpload demo logs: %v\n", err)
			return ExitError
		}
		return ExitPass
	}
}

// demoTag is the demo image tag: $MCPLOAD_DEMO_TAG, else the tag published
// with this mcpload version (latest for development builds).
func demoTag() string {
	if t := os.Getenv("MCPLOAD_DEMO_TAG"); t != "" {
		return t
	}
	return demo.ImageTag(Version)
}

func demoUp(ctx context.Context, yaml string, o demoOpts, stdout, stderr io.Writer) int {
	prefix := os.Getenv("MCPLOAD_DEMO_IMAGE_PREFIX")
	if prefix == "" {
		prefix = demo.DefaultImagePrefix
	}
	fmt.Fprintf(stderr, "mcpload demo: starting the demo servers (images %s-*:%s, project %s)\n", prefix, demoTag(), o.project)
	if err := demoCompose(ctx, yaml, o.project, []string{"up", "-d"}, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "mcpload demo up: %v\n", err)
		fmt.Fprintf(stderr, "mcpload demo up: if a port is taken, stop what uses it or pick other ports with --base-port (e.g. --base-port 13001)\n")
		return ExitError
	}
	fmt.Fprintf(stderr, "mcpload demo: waiting for %d servers to answer /healthz (up to %s)\n", len(demo.Targets), o.timeout)
	healthy := waitHealthy(ctx, o.basePort, o.timeout)
	ok := true
	for _, h := range healthy {
		ok = ok && h
	}
	if ok {
		fmt.Fprintf(stdout, "\nThe mcpload demo servers are up (project %s). They listen on 127.0.0.1 only; don't expose them.\n\n", o.project)
	} else {
		fmt.Fprintf(stdout, "\nSome demo servers did not answer within %s:\n\n", o.timeout)
	}
	printTargets(stdout, o.basePort, healthy, !ok)
	if !ok {
		fmt.Fprintf(stdout, "\nSee why with: mcpload demo logs%s <target>\n", demoFlagsHint(o, false))
		return ExitError
	}
	base := o.basePort
	fmt.Fprintf(stdout, "\nTry:\n")
	fmt.Fprintf(stdout, "  mcpload run --url %s --duration 1m --html report.html\n", demo.Targets[0].URL(base))
	fmt.Fprintf(stdout, "  mcpload run --url %s --scenario lb-check --html lb.html\n", demo.Targets[3].URL(base))
	fmt.Fprintf(stdout, "\nStop them with: mcpload demo down%s\n", demoFlagsHint(o, false))
	return ExitPass
}

// demoFlagsHint repeats non-default flags in the commands mcpload suggests
// (the port only where it matters).
func demoFlagsHint(o demoOpts, withPort bool) string {
	var s string
	if o.project != demo.DefaultProject {
		s += " --project-name " + o.project
	}
	if withPort && o.basePort != demo.DefaultBasePort {
		s += fmt.Sprintf(" --base-port %d", o.basePort)
	}
	return s
}

func printTargets(w io.Writer, base int, healthy []bool, showState bool) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if showState {
		fmt.Fprintln(tw, "  URL\tTARGET\tSTATE\tDEMONSTRATES")
	} else {
		fmt.Fprintln(tw, "  URL\tTARGET\tDEMONSTRATES")
	}
	for i, t := range demo.Targets {
		if showState {
			state := "up"
			if !healthy[i] {
				state = "DOWN"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", t.URL(base), t.Service, state, t.Shows)
		} else {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", t.URL(base), t.Service, t.Shows)
		}
	}
	tw.Flush()
}

// probeAll checks every target's /healthz once.
func probeAll(ctx context.Context, base int) []bool {
	healthy := make([]bool, len(demo.Targets))
	var wg sync.WaitGroup
	for i, t := range demo.Targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			healthy[i] = demoHealthy(ctx, t.HealthURL(base))
		}()
	}
	wg.Wait()
	return healthy
}

// waitHealthy polls every target's /healthz until all answer or the timeout.
func waitHealthy(ctx context.Context, base int, timeout time.Duration) []bool {
	deadline := time.Now().Add(timeout)
	healthy := make([]bool, len(demo.Targets))
	for {
		var wg sync.WaitGroup
		for i, t := range demo.Targets {
			if healthy[i] {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				healthy[i] = demoHealthy(ctx, t.HealthURL(base))
			}()
		}
		wg.Wait()
		all := true
		for _, h := range healthy {
			all = all && h
		}
		if all || !time.Now().Before(deadline) {
			return healthy
		}
		time.Sleep(demoPoll)
	}
}

func httpHealthy(ctx context.Context, url string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

var errNoDocker = errors.New("docker was not found on PATH. The demo servers run in Docker: install Docker Desktop (Windows, macOS) or Docker Engine with the Compose plugin (Linux), see https://docs.docker.com/get-docker/")

func checkDockerCompose() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errNoDocker
	}
	var out bytes.Buffer
	cmd := exec.Command("docker", "compose", "version")
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("`docker compose version` failed (%v): the demo needs Docker Compose v2 (the `docker compose` plugin; the old `docker-compose` is not enough), see https://docs.docker.com/compose/install/\n%s",
			err, strings.TrimSpace(out.String()))
	}
	return nil
}

func runDockerCompose(ctx context.Context, yaml, project string, args []string, stdout, stderr io.Writer) error {
	full := append([]string{"compose", "--project-name", project, "--file", "-"}, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Stdin = strings.NewReader(yaml)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w", strings.Join(full, " "), err)
	}
	return nil
}
