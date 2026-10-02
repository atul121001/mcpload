# mcpload

**Find out whether your MCP server can handle real AI-agent traffic, before your users find out it can't.**

[![ci](https://github.com/atul121001/mcpload/actions/workflows/ci.yml/badge.svg)](https://github.com/atul121001/mcpload/actions/workflows/ci.yml)
[![license](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

mcpload is a free, open-source tool that puts your MCP server through a realistic workout. It sends traffic the way AI agents do, watches how the server holds up over time, and gives you a plain pass or fail with the reasons.

> **Status:** early release. Commands and options may change before 1.0.

---

## Why you'd want this

An MCP server is how AI agents use your product. Agent traffic is not like normal API traffic. An agent opens a session, asks for your list of tools, and then calls several tools at once, again and again. If the server slows down, leaks memory, or loses sessions, every agent that depends on it fails.

Problems like these usually don't show up in a quick manual test. They show up after an hour of real traffic, or once you put two servers behind a load balancer. mcpload is built to catch them before release.

## What it checks

| The question you have | What mcpload does |
|---|---|
| **Can my server handle many agents at once?** | Simulates many agents opening sessions, listing tools and calling tools in parallel, then measures speed and errors for **each tool separately**. |
| **Does it slowly run out of memory?** | Runs a long test (30–60 minutes is typical), samples the server's memory as it goes, and tells you if memory keeps climbing and never comes back down. |
| **Does it lose sessions behind a load balancer?** | Spots the classic "session not found" failure when requests from one agent land on different servers. |
| **Is it ready for the newer stateless MCP spec?** | Speaks both the older session-based protocol and the stateless 2026-07-28 protocol, and picks the right one automatically. |
| **Do logins survive under load?** | Tests OAuth token refresh with many agents at once. |
| **Did my latest change make it slower?** | Runs in CI on every pull request and fails the build if a tool goes over its time or error budget. |

## What you get

Every run ends with a clear verdict in the terminal, an exit code your CI understands, and a report you can open in any browser (`report.html`) or keep as data (`report.json`).

Here is what a failing run says about a server with a memory leak (from the [synthetic example report](report/examples/leaky.json)):

```text
  FAIL     memory_leak        RSS grew 3.09 MiB/min (R²=0.96) under constant load, above the 1 MiB/min limit,
                              and did not recover in cool-down.
  FAIL     session_leak       Active sessions grew 3.01/min (R²=1.00) under constant load, above the 0.5/min
                              limit, and did not recover in cool-down.
  PASS     fd_leak            Open file descriptors flat under constant load.
  PASS     latency_drift      Client p95 stable.
  PASS     error_drift        Error rate did not trend upward over the load window.
  PASS     session_not_found  No 404 session-not-found responses.
  FAIL     threshold          1 of 16 thresholds failed: mcp_req_duration{tool:search} p(99)<2000 (observed 2310).
mcpload result: FAIL
```

(Long lines are wrapped and some messages shortened here.)

In plain words: memory and open sessions kept growing while the load stayed the same, they didn't drop after the load stopped, and the `search` tool was too slow for 1% of calls.

---

## Try it in 5 minutes

The repo includes small demo MCP servers, some healthy and some deliberately broken, so you can see both a pass and a fail without touching a real server.

**You need:** [Docker](https://docs.docker.com/get-docker/), [Go 1.26+](https://go.dev/dl/) and Git.

**1. Get the code**

```bash
git clone https://github.com/atul121001/mcpload.git
cd mcpload
```

**2. Start the demo servers.** They run only on your own machine (127.0.0.1).

```bash
docker compose -f demo-servers/docker-compose.yml up -d --build
```

**3. Build the two tools.** This makes `k6` (the load generator, with MCP support built in) and `mcpload` (the command you'll actually use). It takes a minute the first time.

```bash
go install go.k6.io/xk6@latest
xk6 build --with github.com/atul121001/mcpload/xk6-mcpload=./xk6-mcpload --output ./k6
(cd cmd/mcpload && go build -o ../../mcpload .)
```

**4. Test a healthy server.** One minute of agent traffic:

```bash
./mcpload run --url http://localhost:3001/mcp --scenario scenarios/agent-session.js \
  --k6 ./k6 --duration 1m --out report.json --html report.html
```

You should see a **PASS**. Open `report.html` in your browser to see per-tool timings.

**5. Now test a broken one.** This server sits behind a load balancer that forgets which agent belongs to which server:

```bash
./mcpload run --url http://localhost:3004/mcp --scenario scenarios/lb-check.js \
  --k6 ./k6 --out lb.json --html lb.html
```

You should see a **FAIL** with `session_not_found`. That's the tool catching a real class of bug.

> **On Windows,** the commands are the same with `k6.exe` and `mcpload.exe`. [docs/QUICKSTART.md](docs/QUICKSTART.md) has PowerShell versions of everything.

When you're done: `docker compose -f demo-servers/docker-compose.yml down`

---

## Test your own server

> **Only test servers you own, or have written permission to test.** Start with a staging server, not production.

**1. Point it at your server.** Use your server's MCP URL. If it needs a login token, pass it with `--env`:

```bash
./mcpload run --url https://staging.example.com/mcp \
  --env MCP_TOKEN=your-token \
  --scenario scenarios/agent-session.js --k6 ./k6 \
  --vus 5 --duration 2m --out report.json --html report.html
```

`--vus 5` means 5 simulated agents at the same time. Start small and raise it step by step.

**2. Choose which tools to call.** By default mcpload calls **every tool your server lists**, with sample arguments. If any of your tools change data (create, update, delete, send, pay), limit the test to safe, read-only tools:

```bash
--env 'TOOL_MIX={"search":5,"get_document":3}'                    # tools to call, and how often
--env 'TOOL_ARGS={"search":{"query":"invoices"},"get_document":{"id":"42"}}'   # their arguments
```

**3. Set your budgets.** Decide how fast "fast enough" is:

```bash
--env P95_MS=800 --env P99_MS=2000 --env ERR_RATE=0.01
```

That means: 95% of calls under 800 ms, 99% under 2 seconds, and fewer than 1% errors, **per tool**. One tool can get its own limits with `TOOL_BUDGETS`.

**4. Look for leaks with a long run.** A soak test keeps the load steady for a while, then stops and checks that the server recovers. For memory checks, mcpload needs a way to read your server's memory. You can use either of these:

- your server's Prometheus metrics endpoint (`--sampler prometheus --prom-url .../metrics`), or
- the Docker container it runs in (`--sampler docker --container my-mcp-server`).

```bash
./mcpload run --url https://staging.example.com/mcp --scenario scenarios/soak.js --k6 ./k6 \
  --env MCP_TOKEN=your-token --soak-min 30 \
  --sampler prometheus --prom-url https://staging.example.com/metrics \
  --out soak.json --html soak.html
```

Without a sampler, mcpload still reports speed and error trends, but it can't judge memory.

Every option is listed in [scenarios/README.md](scenarios/README.md) and [cmd/mcpload/README.md](cmd/mcpload/README.md).

---

## Reading the result

| You see | It means |
|---|---|
| **PASS**, exit code `0` | Every check and every budget passed. |
| **FAIL**, exit code `1` | At least one check or budget failed. The report says which, and why. |
| exit code `2` | The test itself couldn't run, for example a wrong URL or k6 not found. |

The checks, in plain words:

| Check | Fails when… |
|---|---|
| `memory_leak` | Memory keeps rising under steady load and doesn't come back down afterwards. |
| `session_leak` | Open sessions pile up and aren't cleaned up. |
| `fd_leak` | Open files or connections pile up. |
| `latency_drift` | Calls get noticeably slower the longer the test runs (a warning). |
| `error_drift` | Errors become more frequent over time (a warning). |
| `session_not_found` | The server "forgets" an agent's session, which often means a load-balancer problem. |
| `threshold` | A tool went over your time or error budget. |

---

## Run it on every pull request

The GitHub Action builds everything, runs the test, posts a pass/fail summary with a per-tool table on the pull request, and saves the report as a download.

```yaml
jobs:
  mcp-load:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      pull-requests: write          # only needed for the PR comment
    steps:
      - uses: actions/checkout@v4
      - run: docker compose up -d --build        # start your MCP server
      # No release is tagged yet. Pin a tag or commit SHA once releases exist.
      - uses: atul121001/mcpload/action@main
        with:
          url: http://localhost:8080/mcp
          scenario: agent-session
          vus: '10'
          duration: 2m
          p95-ms: '800'
          p99-ms: '2000'
          err-rate: '0.01'
          comment-on-pr: 'true'
```

All inputs and outputs are documented in [action/action.yml](action/action.yml). Working examples: [PR gate](.github/workflows/example-pr-gate.yml) and [nightly soak](.github/workflows/example-nightly-soak.yml).

---

## Common questions

**Do I need to know k6?**
No. The ready-made scenarios cover the common cases, and you control them with flags and `--env` settings. If you do know k6, you can write your own scripts (see below).

**Will it break my server?**
It can, if you push it hard. That's the point of a load test, so use staging and start with a few agents. Be careful with tools that change data: limit the test with `TOOL_MIX`.

**Does it send my data anywhere?**
No. Everything runs on your machine or your CI runner. Reports stay local unless you pass `--upload-url` to send one to a server you choose. Tool arguments and results aren't stored in reports.

**Which MCP servers does it work with?**
Remote MCP servers over streamable HTTP, in any language. It has been tested against servers built with the official TypeScript, Python and Go SDKs. Local stdio servers aren't supported.

**How long should a soak test be?**
30–60 minutes catches most slow leaks. A few minutes is enough to check that everything is wired up.

---

## For advanced users

<details>
<summary><b>Write your own scenarios (JavaScript)</b></summary>

mcpload's MCP support is a k6 extension, so you can script any traffic pattern:

```js
import mcp from 'k6/x/mcpload';

const client = new mcp.Client({
  url: __ENV.MCP_URL,
  protocol: 'auto',                       // or '2026-07-28', '2025-11-25', ...
  auth: { type: 'bearer', token: __ENV.MCP_TOKEN },
});

export const options = {
  vus: 10, duration: '1m',
  thresholds: { 'mcp_req_duration{tool:search}': ['p(95)<800'], 'mcp_tool_error_rate': ['rate<0.01'] },
};

export default function () {
  const s = client.connect();
  s.listTools();
  s.callParallel([{ name: 'search', args: { query: 'invoices' } }, { name: 'fast', args: {} }]);
  s.close();
}
```

Full API: [xk6-mcpload/README.md](xk6-mcpload/README.md).
</details>

<details>
<summary><b>Metrics</b></summary>

| Metric | Type | What it measures |
|---|---|---|
| `mcp_req_duration` | Trend | each JSON-RPC round trip (tagged by `method`, `tool`, `status`, `error_type`) |
| `mcp_req_ttfb` | Trend | time to the first response byte |
| `mcp_stream_duration` | Trend | SSE responses: headers to matching event |
| `mcp_connect_duration` | Trend | the whole `connect()` (`initialize` or `server/discover`) |
| `mcp_oauth_refresh_duration` | Trend | OAuth token fetch |
| `mcp_reqs` | Counter | requests |
| `mcp_errors` | Counter | errors by `error_type`: `http`, `jsonrpc`, `tool_iserror`, `timeout`, `session_not_found`, `header_mismatch`, `auth` |
| `mcp_tool_error_rate` | Rate | failed `tools/call` (including `isError: true`) |
| `mcp_sessions_open` | Gauge | client-side open sessions |

Report format: [report/schema/README.md](report/schema/README.md).
</details>

<details>
<summary><b>Built-in scenarios</b></summary>

| Scenario | What it simulates |
|---|---|
| `agent-session.js` | Agents opening a session, listing tools and calling several in parallel, with pauses in between. |
| `burst.js` | A sudden rush of new agents, including a flood of session starts. |
| `soak.js` | Steady traffic for a long time, then a quiet period, to find leaks. |
| `lb-check.js` | Session handling behind a load balancer. |
| `oauth-refresh.js` | Many agents sharing short-lived login tokens. |

Settings for each: [scenarios/README.md](scenarios/README.md).
</details>

<details>
<summary><b>Repository layout</b></summary>

| Path | What |
|---|---|
| [`xk6-mcpload/`](xk6-mcpload/README.md) | the k6 extension that speaks MCP (Go) |
| [`cmd/mcpload/`](cmd/mcpload/README.md) | the `mcpload` command: runs k6, samples the server, computes verdicts, writes the report |
| [`scenarios/`](scenarios/README.md) | ready-made test scenarios |
| [`report/`](report/schema/README.md) | report format, validator and HTML report |
| [`demo-servers/`](demo-servers/README.md) | healthy and deliberately broken demo servers (local-only, intentionally vulnerable) |
| [`action/`](action/action.yml) | the GitHub Action |
</details>

---

## Learn more

- [Quick start for Windows, Linux and macOS](docs/QUICKSTART.md)
- [How it works (architecture)](docs/ARCHITECTURE.md)
- [Calibration results and recommended budgets](docs/CALIBRATION.md)
- [Roadmap](docs/ROADMAP.md)
- [Contributing](CONTRIBUTING.md)

## License

[Apache-2.0](LICENSE)
