# Test your own server

[README](../../README.md) · [All docs](../README.md)

> **Only test servers you own, or have written permission to test.** Start with a staging server, not production.

**1. Point it at your server.** Use your server's MCP URL. If it needs a login token, pass it with `--env`:

```bash
mcpload run --url https://staging.example.com/mcp \
  --env MCP_TOKEN=your-token \
  --vus 5 --duration 2m --html report.html
```

`--vus 5` means 5 simulated agents at the same time. Start small and raise it step by step.

**2. Choose which tools to call.** By default mcpload calls **every tool your server lists**, with sample arguments. If any of your tools change data (create, update, delete, send, pay), limit the test to safe, read-only tools:

```bash
--env 'TOOL_MIX={"search":5,"get_document":3}'                    # tools to call, and how often
--env 'TOOL_ARGS={"search":{"query":"invoices"},"get_document":{"id":"42"}}'   # their arguments
```

If you name a tool in `TOOL_MIX` that your server doesn't list (a typo, say), mcpload prints a warning and records a failed check, so a misspelled tool shows up in the result instead of quietly going untested.

> **Windows PowerShell 5.1** (the `powershell` that comes with Windows) strips the double quotes out of JSON before mcpload sees it. Escape each one with a backslash there: `--env 'TOOL_MIX={\"search\":5,\"get_document\":3}'`. PowerShell 7.3 or newer (`pwsh`) passes the quotes correctly, so use the plain form above, the same as on Mac or Linux.

**3. Set your budgets.** Decide how fast "fast enough" is:

```bash
--env P95_MS=800 --env P99_MS=2000 --env ERR_RATE=0.01
```

That means: 95% of calls under 800 ms, 99% under 2 seconds, and fewer than 1% errors, **for every tool the test calls**. Each tool is judged on its own; one slow tool can't hide behind fast ones. Speed is measured over the calls that succeeded, and failed calls count toward the error rate.

To give one tool different limits, use `TOOL_BUDGETS`. Tools you don't mention keep the defaults above:

```bash
--env 'TOOL_BUDGETS={"generate_report":{"p95":3000,"p99":6000}}'
```

**4. Look for leaks with a long run.** A soak test keeps the load steady for a while, then stops (the "cool-down") and checks that the server recovers. Leak checks only run on soak tests: a short test can't tell a leak from a server that's still warming up, so on a normal run they show as skipped. Plan on **at least 10 minutes** of steady load; shorter soaks only reliably catch fast leaks (around 2 MiB per minute or more).

For memory checks, mcpload also needs a way to read your server's memory. You can use either of these:

- your server's Prometheus metrics endpoint (`--sampler prometheus --prom-url .../metrics`), or
- the Docker container it runs in (`--sampler docker --container my-mcp-server`).

```bash
mcpload run --url https://staging.example.com/mcp --scenario soak \
  --env MCP_TOKEN=your-token --soak-min 30 \
  --sampler prometheus --prom-url https://staging.example.com/metrics \
  --out soak.json --html soak.html
```

The soak starts 2 new agent sessions per second by default. Change it with `--env RATE=...`; fractions work too, so `--env RATE=0.05` means one new session every 20 seconds, a gentle pace for a small staging server.

Without a sampler, mcpload still reports speed and error trends, but it can't judge memory. If your Prometheus endpoint also reports heap size (as Node, Go and Python clients usually do), the memory check looks at the heap as well as total memory.

To find how many agents your server can take, see [Find your capacity](capacity.md). To test whole business flows instead of single tools, see [Workload profiles](workloads.md).

## Getting trustworthy results

A load test measures the server *and* the computer sending the load. A few habits keep the numbers honest:

- **Run mcpload on a different machine from the server**, or at least on different CPU cores. If both fight over the same CPU, a healthy server can look slow.
- **Start small.** Begin with a few agents and a short run, check that everything passes, then raise the load step by step. That way you learn where the server starts to struggle instead of just seeing a wall of errors.
- **Hunt leaks with a long soak and a sampler.** Use at least 10 minutes of steady load (30–60 is better) with `--sampler docker` or `--sampler prometheus`. Without a sampler there's nothing to judge memory by.
- **Watch the `generator` check.** It tells you when the load generator itself was the bottleneck: it couldn't send all the traffic you asked for, or it was using most of its CPU. If it warns or fails, the speed numbers may be partly the test machine's fault, not the server's. Give mcpload a bigger machine or lower the load, and run again.

## Run with Docker

Nothing to install but Docker: the `ghcr.io/atul121001/mcpload` image contains mcpload and all the bundled scenarios. Mount a folder at `/work` and the reports are written there:

```bash
docker run --rm -v "$PWD:/work" ghcr.io/atul121001/mcpload \
  run --url https://staging.example.com/mcp --vus 5 --duration 2m --html report.html
```

Every `mcpload` command and flag works the same way; scenarios can be named (`--scenario soak`) or read from the mounted folder (`--scenario ./my-test.js`). Pin a version with `ghcr.io/atul121001/mcpload:<version>` (for example `:0.6.0`) in CI.

To test a server on your own machine, such as the demo servers:

- **Windows and Mac (Docker Desktop):** use `host.docker.internal` in place of `localhost`, e.g. `--url http://host.docker.internal:3001/mcp`.
- **Linux:** add `--network host` and keep `localhost` (the demo servers listen on 127.0.0.1 only). For a server that listens on all interfaces you can instead add `--add-host=host.docker.internal:host-gateway` and use `host.docker.internal`. Add `--user "$(id -u):$(id -g)"` so the reports belong to you, not root.

```bash
# Linux
docker run --rm --network host --user "$(id -u):$(id -g)" -v "$PWD:/work" ghcr.io/atul121001/mcpload \
  run --url http://localhost:3001/mcp --duration 1m --html report.html
```

```powershell
# Windows PowerShell
docker run --rm -v "${PWD}:/work" ghcr.io/atul121001/mcpload run --url http://host.docker.internal:3001/mcp --duration 1m --html report.html
```

`--sampler docker` and `--chaos-restart` call the `docker` command, which the image doesn't include; use `--sampler prometheus` from a container, or run mcpload directly.
