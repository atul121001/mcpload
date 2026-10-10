# Local (stdio) servers

[README](../../README.md) · [All docs](../README.md)

Most MCP servers people install today are local: a desktop client (Claude Desktop, an IDE, an agent framework) starts the server as a subprocess and talks to it over **stdin and stdout**. mcpload can test those servers too. You give it the command that starts your server instead of a URL:

```bash
mcpload run --command "node server.mjs --stdio" --vus 5 --duration 2m --html report.html
```

Everything that works over HTTP above the wire works the same way: tool mixes, per-tool budgets, agent workflows, workload profiles, cancellation, sampling and elicitation answers, resources and prompts, `mcpload capacity`.

## How mcpload runs your server

- **One session is one process**, as in desktop MCP clients. Every time a simulated agent connects, mcpload starts your command; when the agent closes the session, mcpload closes the server's stdin, waits briefly for it to exit, then stops it. `--vus 20` means up to 20 copies of your server running at once, on the machine that runs mcpload.
- **The command is started directly, never through a shell.** No pipes, `&&`, globbing or `$VARS` in `--command`. Launchers such as `npx` and `uvx` work; mcpload stops the whole process tree they start.
- **Environment and folder.** The server inherits mcpload's environment, except mcpload's own credentials (`MCPLOAD_KEY`, `MCP_TOKEN`, `OAUTH_CLIENT_SECRET`). Add variables with `--command-env KEY=VALUE` (repeatable) and set its working folder with `--command-cwd DIR`.
- **Handshake.** `initialize` is sent straight away (the `server/discover` probe is an HTTP rule). Cancellation sends `notifications/cancelled` on stdin.
- `--transport stdio` (or `http`, default `auto`) makes the choice explicit.

```bash
# a local script
mcpload run --command "node build/index.js" --command-cwd ./my-server

# a published package, through a launcher
mcpload run --command "npx -y @your-org/your-mcp-server" --command-env API_BASE=https://staging.example.com
mcpload run --command "uvx your-mcp-server"

# a server image
mcpload run --command "docker run -i --rm your-registry/your-mcp-server:1.2.3"
```

With launchers, the first start may download the package and is much slower than the next ones. Run the command once by hand (or a short warm-up run) before you measure startup. With `docker run`, keep `-i` (stdin) and `--rm`, and make sure the server exits when its stdin closes; otherwise a container can outlive its session. Container start-up then counts as server start-up.

Not available with stdio: `lb-check` and `version-skew` (there is no load balancer between a client and its subprocess), `--chaos-restart` (there is no container to restart; the server is restarted on every new session anyway) and `--calls-url`.

## What "load" means for a stdio server

A stdio server has one client. It never sees 100 agents on one process, so the questions are different from HTTP:

**1. How expensive is it to start?** Every session starts a process: runtime start-up, imports, config loading, a database or API connection, then `initialize`. `mcp_process_spawn_duration` (until the process is running) and `mcp_connect_duration` (until `initialize` has answered) show it per session. `burst` and the default `agent-session` start many sessions, so they show how start-up behaves when many agents (or many editor windows) start servers at once.

**2. Do calls block each other inside one process?** An agent fires several tool calls at once (`PARALLEL`, default 3), and they all go to the same process. A server that does synchronous work (CPU-heavy parsing, a sync file or database call, a blocking SDK) on a single-threaded runtime makes every other call wait: head-of-line blocking. The `isolation` scenario measures it by running your tools alone and then mixed with your slow tools:

```bash
mcpload run --command "node server.mjs" --scenario isolation --env SLOW_TOOLS=generate_report --env PARALLEL=5
```

**3. Does one long session leak?** A desktop client keeps one server process for hours. For stdio, mcpload's default sampler is `process`: it reads the memory (RSS) and open file descriptors of the server processes it started, so you don't need Prometheus or Docker. Use `long-lived`, which keeps one session (one process) per agent for `SESSION_MIN` minutes, to see a process that grows with the calls it serves:

```bash
mcpload run --command "node server.mjs" --scenario long-lived --env SESSION_MIN=20 --vus 5
```

`soak` starts a new process for every session, so it tests something else: whether processes start and exit cleanly over time (`mcp_processes_open` should stay flat, and memory should come back down in the cool-down).

**4. How many copies fit on one machine?** Raising `--vus` (or `mcpload capacity`) runs more server processes side by side on the same host. That tells you how many agents or editor windows one machine can serve before CPU or memory runs out; it is a host limit, not a server limit.

## Checks that only stdio has

- **`stdout_pollution`.** stdout carries the protocol. A `console.log`, a `print()` or a library banner on stdout puts a line there that is not JSON-RPC. Real clients may drop the connection or hang on it; mcpload skips such lines, counts them (`mcp_stdout_invalid_lines`) and reports them in this verdict. Fix: log to stderr.
- **`process_exit`.** A server process that exits while its session is still in use (a crash, an unhandled exception, an out-of-memory kill) fails every pending call with error type `process_exit`. `mcp_process_exits` counts exits tagged `expected` (the session was closing) and `exit_code`. The last part of the server's stderr is included in the error message, so the crash reason shows up in the run output.

Every sample is tagged `transport` (`stdio` or `http`), and `report.json` records the command and transport under `run.target`.

## Read the numbers with care

- **They measure your process and this host, not a network.** There is no TLS, proxy or network hop. The load generator and every server process share the same CPU and memory, so a busy machine slows both down. Close other heavy programs, and compare runs only on the same machine.
- **stdio latency is not comparable with HTTP latency.** A `tools/call` over a pipe skips the HTTP stack, so it is usually faster than the same server over HTTP; agent-session runs also pay process start-up in every session. Budgets that fit your HTTP deployment don't automatically fit stdio, and the other way round.
- **One process serves one agent.** A server that handles 20 agents over HTTP in one process may behave very differently as 20 separate processes (more memory in total, no shared caches, no shared pool). Test the transport your users actually run.

## Try it with the demo server

The TypeScript demo server has a stdio mode with four personas. From a clone of the repository:

```bash
cd demo-servers/ts-server && npm ci && cd ../..
S="node demo-servers/ts-server/server.mjs --stdio"

mcpload run --command "$S"                                          # normal: expected to pass
mcpload run --command "$S --persona blocking" --scenario isolation  # sync CPU work: head-of-line blocking
mcpload run --command "$S --persona leaky" --scenario long-lived    # retains ~100 KB per tool call
mcpload run --command "$S --persona noisy"                          # logs to stdout: stdout_pollution
```

The persona can also be set with `--command-env PERSONA=blocking`. Details in the [demo servers README](../../demo-servers/README.md#stdio-mode-ts-image).

## Security

`--command` runs a program on your machine with your permissions and your environment, exactly as if you ran it yourself, once per session. Only give it servers you would run yourself. The command line, including its arguments, is recorded in `report.json` and printed in the run output, so **don't put secrets in the arguments**: pass them with `--command-env` or export them before starting mcpload. Values of `--command-env` are never logged or written to the report. See the [threat model](../THREAT_MODEL.md#12-local-stdio-servers-mcpload-runs-your-command).
