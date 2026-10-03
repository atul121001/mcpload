# mcpload CLI

`mcpload` wraps a k6 binary built with [`xk6-mcpload`](../../xk6-mcpload). It runs a scenario, samples the server's resources while the run is going, computes leak and drift verdicts, and writes `report.json` ([schema v1](../../report/schema/README.md)) and a self-contained `report.html`.

## Build

```sh
cd cmd/mcpload
go build -o mcpload.exe .          # Windows; use -o mcpload elsewhere
go build -ldflags "-X main.version=v0.3.0" -o mcpload .   # stamp a version
go vet ./... && go test ./...
```

`mcpload` needs the custom k6 binary. By default it uses `./k6.exe` or `./k6` in the working directory, then next to the mcpload executable, then `k6` on `PATH`. Use `--k6` to point at a different one. The HTML template is embedded, so Node isn't needed.

## Commands

```text
mcpload run --url <mcp url> [--scenario <file.js|name>] [flags]
mcpload render <report.json> <out.html>
mcpload validate <report.json>
mcpload upload --url <upload server base url> --key <api key> <report.json>
mcpload version
```

### Exit codes

| Code | Meaning |
|---|---|
| 0 | The run passed: no verdict is `fail` and every threshold passed. Warnings are allowed. |
| 1 | The run failed: a verdict is `fail` or a k6 threshold failed. `report.json` and `report.html` are still written. |
| 2 | Usage or runtime error, such as bad flags, k6 not found, the sampler probe failing, k6 crashing, a failed upload, or a report that fails its semantic checks. |

### `run` flags

| Flag | Default | Meaning |
|---|---|---|
| `--scenario` | `agent-session` | k6 script path (e.g. `scenarios/soak.js`, used as is) or a bundled scenario name (e.g. `soak`, `lb-check`). See [Choosing a scenario](#choosing-a-scenario). |
| `--url` | (required) | MCP endpoint, passed to k6 as `MCP_URL` |
| `--protocol` | `auto` | `MCP_PROTOCOL` |
| `--k6` | `./k6.exe`, `./k6`, next to mcpload, then `k6` on PATH | k6 binary built with xk6-mcpload |
| `--sampler` | `none` | `none`, `docker` or `prometheus` |
| `--container` | | container name or id, for `--sampler docker` |
| `--prom-url` | | Prometheus text endpoint, e.g. `http://localhost:3001/metrics`, for `--sampler prometheus` |
| `--interval` | `10s` | sampling interval, which is also the width of the series buckets (use ≥ 5s with docker) |
| `--soak-min` | 30 | `SOAK_MIN`: constant-load minutes |
| `--warmup-min` | 10% of soak, at least 1 | `WARMUP_MIN` |
| `--cooldown-min` | 5 | `COOLDOWN_MIN` |
| `--vus` | | passed to the script as `VUS`, a scenario knob. It is **not** k6 `--vus`, which would replace the script's scenarios. |
| `--duration` | | passed to the script as `DURATION`, a scenario knob |
| `--min-agents` | | step-load: the concurrency the server must hold within budgets for the `capacity` verdict to pass, passed as `MIN_AGENTS`. See [Step load and the capacity verdict](#step-load-and-the-capacity-verdict). |
| `--env K=V` | | extra `-e K=V` for k6, repeatable (e.g. `--env RATE=1 --env THINK_MS=200`). Explicit flags win over `--env`. |
| `--label` | | short display name for the target |
| `--git-sha`, `--git-ref` | `$GITHUB_SHA`, `$GITHUB_REF` | revision of the system under test. The sha must be 7–64 lowercase hex characters. |
| `--out` | `report.json` | report path |
| `--html` | | also write the HTML report here |
| `--upload-url`, `--key` | key from `$MCPLOAD_KEY` | POST the report to `{url}/api/v1/runs` on a server that accepts report uploads, once it is written |
| `--leak-slope-mb-per-min` | 1 | `memory_leak` RSS slope limit in MiB/min (1 MiB = 1,048,576 bytes; the flag name says "mb" for compatibility) |
| `--min-r2` | 0.7 | minimum R² for a leak slope to count |
| `--include-payloads` | false | sets `INCLUDE_PAYLOADS=1` and `payloadsIncluded: true` |
| `--k6-out` | (temp dir, deleted) | keep k6's raw `metrics.ndjson` and `summary.json` in this directory |
| `--wait-ready` | `0` (off) | before starting, wait up to this long (e.g. `2m`) for the server to answer an MCP handshake. See [How `run` works](#how-run-works). |
| `--chaos-restart` | `0` (off) | run `docker restart` on `--chaos-container` this long after k6 starts (e.g. `30s`). **Only on servers you own.** See [Chaos restart and the recovery verdict](#chaos-restart-and-the-recovery-verdict). |
| `--chaos-container` | `--container` | the container `--chaos-restart` restarts. mcpload never restarts a container that wasn't named. |
| `--workload` | | a workload profile (YAML or JSON): weighted business flows, test data and per-flow budgets. mcpload validates it before k6 starts (errors name the flow, step and field), reads its CSV files relative to it, passes a normalized copy to k6 as `WORKLOAD_FILE`, and runs the `workload` scenario unless `--scenario` is given. Adds the per-flow table, `report.json` `workload` and the `workload` verdict. See [Workload profiles](../../scenarios/README.md#workload-profiles). |
| `--calls-url` | | the server's record of executed call ids (`GET <url>?prefix=<p>` → `{"executions": {"<call id>": count}}`), e.g. `http://localhost:3019/calls`, for the `call_integrity` verdict |

`--soak-min`, `--warmup-min`, `--cooldown-min`, `--vus` and `--duration` reach k6 only when you set them. When a flag is left out, the script uses its own default. The CLI computes report phases with the same defaults as `scenarios/soak.js`.

Scenario knobs can also come from the shell environment (`SOAK_MIN=4 RATE=5 ./mcpload run ...`). `k6 run` passes the OS environment to the script, so mcpload reads it the same way: `k6 inspect` runs with `--include-system-env-vars`, and report phases use the OS values. The order of precedence, from highest to lowest, is explicit flags, then `--env`, then the OS environment, then the script's defaults.

## Choosing a scenario

The first test needs only the endpoint:

```sh
./mcpload run --url https://your-server/mcp
```

Without `--scenario`, mcpload runs the bundled `agent-session` scenario. It looks for `scenarios/agent-session.js` in the current folder first, then next to the mcpload executable (release archives ship `mcpload`, `k6` and `scenarios/` together, so this works from any folder). A bare name such as `--scenario soak` or `--scenario lb-check` is looked up the same way as `scenarios/<name>.js`. A value that is an existing file, or that looks like a path (has a `/`, `\` or an extension such as `.js`), is used as is, as before. mcpload prints the script it picked (`mcpload: using scenario ...`). If nothing is found it exits 2 and lists the paths it tried.

## Examples

```sh
# Default agent-session scenario, short smoke run
./mcpload run --url http://localhost:3001/mcp --duration 15s

# Healthy target, Prometheus sampler, short soak
./mcpload run --scenario scenarios/soak.js --url http://localhost:3001/mcp \
  --sampler prometheus --prom-url http://localhost:3001/metrics \
  --soak-min 4 --warmup-min 1 --cooldown-min 1 --interval 5s \
  --out healthy.json --html healthy.html

# Leaky target, docker sampler (expect memory_leak: fail, exit 1)
./mcpload run --scenario scenarios/soak.js --url http://localhost:3002/mcp \
  --sampler docker --container mcpload-demo-ts-leaky-1 \
  --soak-min 4 --warmup-min 1 --cooldown-min 1 --interval 5s --env RATE=1 \
  --out leaky.json --html leaky.html

# Breaking point: steps of 5, 10, 20, 40 agents, 20 s each; PASS only if 20 agents hold the budgets
./mcpload run --url http://localhost:3008/mcp --scenario step-load \
  --env STEPS=5,10,20,40 --env STEP_DURATION=20s --min-agents 20 --html steps.html

# Business workload: 60/30/10 mix of flows with CSV test data and per-flow budgets
./mcpload run --url http://localhost:3001/mcp --workload examples/workloads/customer-support.yaml --duration 30s

# Rolling deploy with two builds behind one LB: do mismatched requests fail fast or hang?
./mcpload run --url http://localhost:3010/mcp --scenario version-skew --env MCP_TIMEOUT=5s --sampler none

# Plain load test, client-side signals only
./mcpload run --scenario scenarios/agent-session.js --url http://localhost:3001/mcp --vus 5 --duration 1m

./mcpload validate report.json
./mcpload render report.json report.html
MCPLOAD_KEY=... ./mcpload upload --url https://reports.example.com report.json
```

## How `run` works

1. It resolves the k6 binary and runs `k6 version`. It then runs `k6 inspect --include-system-env-vars` with the same `-e` env to read the script's thresholds, scenarios (for `run.load`) and the `scenario_name` tag (for `run.scenario`; when the tag is missing, the script's file name is used).
2. With `--wait-ready`, it polls the endpoint (every 250 ms, backing off to 2 s) with the same handshake the test uses: `server/discover` for a 2026-07-28+ `--protocol`, `initialize` for an older one, and for `auto` `server/discover` falling back to `initialize`. The server is ready once it answers HTTP 2xx with a JSON-RPC `result` (JSON or SSE); a stateful session the probe opens is closed with a DELETE. Connection errors, timeouts, 5xx, a proxy's 404/502 and JSON-RPC errors mean "not ready yet". The probe sends `MCP_HEADERS` and the `MCP_TOKEN` bearer token. A 401/403 stops the wait at once (exit 2), except when `OAUTH_TOKEN_URL` is set: the probe doesn't fetch an OAuth token, so there a 401/403 counts as ready. If the server isn't ready in time, or on Ctrl-C, mcpload exits 2 without starting k6.
3. For `docker` or `prometheus`, it takes one probe sample and exits 2 if that fails. It then starts sampling every `--interval`.
4. It runs `k6 run --out json=<tmp>/metrics.ndjson --summary-export <tmp>/summary.json --summary-trend-stats ... -e ... <script>`. k6's stdout and stderr stream through. k6 runs in its own process group. On SIGINT (Ctrl-C) or SIGTERM, mcpload forwards the signal to k6 once. On Windows it sends CTRL_BREAK, which k6 handles like Ctrl-C. k6 then stops gracefully. If k6 hasn't exited after 30 s, or on a second signal, mcpload kills it. Either way, mcpload writes the report from the data collected so far. k6 exit codes 0 and 99 (thresholds failed) count as normal. Any other exit code, including 105 for an interrupted run, produces a report but returns exit 2. The exception is 108 (`exec.test.abort`) in a `step-load` run: that is the scenario's `ABORT_ERR_RATE` guard stopping a broken server early, so it counts as normal and the `capacity` verdict says the run stopped early.
   While k6 runs, mcpload also samples the k6 process's CPU time every `--interval`. On Linux it reads `/proc/<pid>/stat`, on Windows `GetProcessTimes`, and on macOS/BSD `ps -o time=`. Each sample becomes a share of the whole machine: `cpu time / (wall time × runtime.NumCPU())`. The result goes in `run.generator`: `cores`, `cpuAvgPct` over the whole k6 process lifetime (exact, from the exited process's user+system time) and `cpuMaxPct` (the busiest full interval).
5. It stops the sampler and streams the NDJSON twice without loading it all into memory. The first pass finds the earliest sample, which becomes the run start and `t = 0`. The second pass computes:
   - client series per bucket: p95 of every `mcp_req_duration` sample; `mcp_errors / mcp_reqs` (`null` when there were no requests); rps (`mcp_reqs / interval`); and `droppedIterations` (k6's `dropped_iterations`);
   - `series.tools.<tool>.p95Ms`: per-bucket p95 of the successful `tools/call` durations of each tool (`null` in buckets without one);
   - `tools[]`: `mcp_reqs`, `mcp_errors` and `mcp_req_duration` with `method=tools/call`, grouped by `tool`, using k6's percentile interpolation. `p50`/`p95`/`p99`/`max` cover successful calls only. xk6-mcpload tags `error_type` only on failed requests, so a sample without it is a success. `reqs`, `errors` and `errorRate` count every call;
   - `summary`: totals and `byErrorType` from the `error_type` tag, `iterations` and `droppedIterations` (k6's built-in counters), and `toolErrors` (`mcp_errors{error_type:tool_iserror}`);
   - `run.protocol`: the most common `protocol` tag on successful `mcp_connect_duration` samples. This skips the `auto` probe. If none is found, `--protocol` is used unless it is `auto`; otherwise the value is `unknown`;
   - `thresholds[]`: one entry per expression from `k6 inspect`, plus any extras in the summary export. `passed` comes from k6's summary export, where `true` means *failed*. `observed` is recomputed from the raw samples, so it is available for any `p(N)`, `avg`, `rate` or `count`.
6. Server points are averaged into the same buckets: `t` = bucket start in seconds from run start. Docker fills only `rssBytes`. Prometheus fills `rssBytes`, `heapBytes` (`nodejs_heap_size_used_bytes`, the prom-client default, falling back to `nodejs_heap_used_bytes`), `openFds` and `activeSessions`, with `null` where a metric is missing.
   There are `ceil(durationS / interval)` buckets. A trailing bucket that covers less than half an interval is dropped from every series, client and server alike. Its rps would otherwise show a false drop and skew the fits. The samples in it still count in `summary` and `tools[]`, and the other buckets don't change.
7. Phases: for `soak`, they come from `SOAK_MIN`, `WARMUP_MIN` and `COOLDOWN_MIN`; for `long-lived`, from `WARMUP_MIN` (1), `SESSION_MIN` (10) and `COOLDOWN_MIN` (2). Any other scenario uses `warmupEndS = 0` and `loadEndS = cooldownEndS = durationS`.
8. Verdicts are `analysis.Verdicts` (memory, session and fd leak, latency and error drift), then `session_not_found`, then `threshold`, then `generator`. The `generator` verdict checks whether k6 kept up: dropped iterations and k6 CPU. A `fail` there gives exit 1 like any other verdict. In a `step-load` run (or any run whose requests carry a `step` tag) mcpload adds `capacity`, writes the per-step table to `report.json` `capacity` and skips `latency_drift` and `error_drift`. A `long-lived` run adds `session_survival`, a run with `--chaos-restart` (or of `reconnect-storm`) adds `recovery`, and a run whose calls carry call ids (or with `--calls-url`) adds `call_integrity`. In a `version-skew` run (or any run that emits the `mcp_skew_*` metrics) it adds `version_skew` (see below). A `workload` run (`--workload`, or any run with a `WORKLOAD_FILE` that emits the `mcp_workload_*` metrics) adds `workload` and writes `report.json` `workload`. mcpload then runs `report.Check()` and writes the JSON and the HTML. It uploads when asked, but not when `report.Check()` failed; in that case it prints why and exits 2. Finally it prints the verdict lines and exits.

Docker (`MemUsage`, which is the cgroup) and Prometheus (`process_resident_memory_bytes`) report RSS on different scales. Compare slopes only within one sampler.

## Version skew and the version_skew verdict

`--scenario version-skew` runs agent sessions against a load balancer whose replicas run different builds and classifies every failed request as `fast` (a typed error in under `FAIL_FAST_MS`, default 2000), `slow` or `hang` (a timeout, or at least `HANG_MS`, default 10000); see [scenarios/README.md](../../scenarios/README.md#version-skew). mcpload reads the scenario's `mcp_skew_*` metrics and judges:

| | Status |
|---|---|
| no request failed | `pass` |
| requests failed, none hung | `warn`: clients see errors they can handle; the exit code stays 0 |
| any request hung | `fail` (exit 1). The scenario's own `mcp_skew_failures{kind:hang}` threshold fails too. |
| the scenario recorded no requests | `skipped` |

The message names the share of failed requests, the counts per kind and reason, the median time to failure, which protocol sessions negotiated with which replica, and how requests spread over the replicas (from the `X-Served-By` response header, or `SERVED_BY_HEADER`):

```text
  WARN     version_skew       3540 of 7525 requests (47.0%) failed on a replica running a different build; 3540 failed fast with typed errors (good: clients can catch them): 3196 × HTTP 400, 344 × Unsupported protocol version (-32022). ...
  FAIL     version_skew       90 of 180 requests (50.0%) failed on a replica running a different build; 90 hung for a median 5 s (bad: clients will hang during a rolling deploy): 90 × no answer before the client timeout. ...
```

## Step load and the capacity verdict

`--scenario step-load` raises the number of concurrent agents in steps (`STEPS`, default `10,25,50,100,200`, each held for `STEP_DURATION`, default `1m`; see [scenarios/README.md](../../scenarios/README.md#step-load)). Every request made while a step holds is tagged `step=<agents>`. mcpload groups the samples by that tag and, for each step, computes the request rate, error rate, connect p95, the share of failed session starts, each tool's p95, p99 and error rate (successful calls only for latency) and the busiest k6 CPU interval within the step.

Each step is judged against the budgets the scenarios use for thresholds, read from the same environment (`--env`, then the OS environment): `P95_MS` (800), `P99_MS` (2000), `ERR_RATE` (0.01), `TOOL_BUDGETS` merged over `{"flaky":{"errRate":0.2}}`, and `CONNECT_P95_MS` (1500). As in k6, a value at or above its budget is a breach. Tools with fewer than 10 calls in a step aren't judged in it. Failed session starts are judged against `ERR_RATE`, and a step without any `tools/call` is a breach.

- **Breaking point**: the first step with a breach. **Max sustainable concurrency**: the step before it, or the highest step when none broke.
- **Inconclusive**: when k6 used more than 90% of the machine's CPU during the breaking step, the breach may be the load generator's own overhead, so the verdict says so instead of blaming the server.
- **Stopped early**: when the scenario's `ABORT_ERR_RATE` guard aborted k6 (exit code 108), the steps that didn't run are listed.

The terminal shows one line per step and a plain summary:

```text
mcpload steps (agents, result, req/s, error rate, connect p95, slowest tool p95, k6 CPU):
       5  PASS        50.2     0.70%     6.4 ms  slow 582 ms             0%
      10  PASS        86.7     0.52%     6.9 ms  slow 687 ms             1%
      20  BREACH      99.8     0.63%      10 ms  slow 1.04 s             1%
          `slow` p95 1.04 s > 800 ms; `flaky` p95 901 ms > 800 ms; ...
max sustainable concurrency: 10 agents (budgets broke at 20)
  FAIL     capacity           Held budgets up to 10 agents; at 20 agents `slow` p95 1.04 s > 800 ms (+4 more). Target MIN_AGENTS=20 not met.
```

How `capacity` affects the result:

| | Status |
|---|---|
| `--min-agents N` (or `MIN_AGENTS`) and max sustainable ≥ N | `pass` |
| `--min-agents N`, a conclusive breach at or below N (or at the first step) | `fail` (exit 1) |
| `--min-agents N`, otherwise (inconclusive at or below N, N above the highest step, or N between the last passing and the breaking step) | `warn` |
| no target, no step broke | `pass` |
| no target, the first step broke conclusively (nothing is sustainable: usually a server that is down or a misconfigured run) | `fail` |
| no target, a later step broke, or inconclusive | `warn`: informational, the exit code stays 0 |
| no request carried a step tag | `skipped` |

The step-load script sets no k6 thresholds, so a breach at a high step never fails the run by itself; set `--min-agents` to gate CI on capacity.

## Long-lived sessions and the session_survival verdict

`--scenario long-lived` holds one session per agent for `SESSION_MIN` minutes (see [scenarios/README.md](../../scenarios/README.md#long-lived-sessions)). mcpload reads `mcp_session_lifetime` and writes `report.json` `sessions`: how many sessions survived and died, deaths by cause, the median lifetime of the ones that died, reconnects, and, for the tool whose latency rose most, the p95 of successful calls in the first and last third of the sessions.

| | `session_survival` |
|---|---|
| a session died before its planned end | `fail`, e.g. "18 of 50 sessions died after a median 5m02s with session_not_found 18: the server dropped live sessions ..." |
| none died, but for some tool (≥ 30 calls early and late) the late p95 is ≥ 1.5× the early p95 and ≥ 25 ms higher | `warn` |
| otherwise | `pass` |
| no long-lived session ended | `skipped` |

## Chaos restart and the recovery verdict

`--chaos-restart 30s --chaos-container <name>` makes mcpload run `docker restart <name>` 30 s after k6 starts, through the docker CLI as the docker sampler does. `--chaos-container` defaults to `--container`; without either, mcpload refuses to start. It prints which container it will restart before the run and again when it does. **Only restart containers you own and are allowed to disrupt.** Pair it with `--scenario reconnect-storm`, whose agents reconnect as soon as their session breaks.

```sh
./mcpload run --scenario reconnect-storm --url http://localhost:3019/mcp --vus 20 --duration 2m \
  --chaos-restart 30s --chaos-container mcpload-chaos-ts --calls-url http://localhost:3019/calls --html storm.html
```

`report.json` gets `chaos`: `{action: "restart", container, ran, atS, durationS, error?, recovery}`. `atS` is when `docker restart` started (seconds since run start); the HTML report draws it, and the recovery point, as vertical lines on the time-series charts. From 100 ms buckets of the client's own requests, mcpload computes `recovery`:

- **recovery point**: the first moment after the restart when requests are served without errors and the following `RECOVERY_WINDOW` (5 s) has requests, an error rate below `ERR_RATE` (tool errors, `isError: true`, left out: they say nothing about reachability) and, if it has successful connects, a connect p95 below `CONNECT_P95_MS`;
- `serverBackS` (first successful connect after the first error), `reconnects` and `lastReconnectS` (agents that got a new session after a break), and the connect attempts, failed connects and errors by type from the restart to the end of the recovery window: the connect flood.

| | `recovery` |
|---|---|
| recovered within `RECOVERY_BUDGET` (30 s) of the restart | `pass`, e.g. "After the restart of mcpload-chaos-ts at 20 s (docker restart took 0.8 s), the server accepted sessions again after 1.3 s and 10 agents reconnected within 2.0 s (42 connect attempts, 32 failed); errors returned to under 1% with connect p95 under 1500 ms 2.0 s after the restart (budget 30 s). Errors during the outage: http 57, session_not_found 3." |
| recovered later, or not before the run ended | `fail` |
| `docker restart` failed | `warn` |
| the run ended before the restart was due | `skipped` |

`session_not_found` errors in that span are expected after a restart (the server lost its sessions), so the `session_not_found` verdict leaves them out and says how many there were.

### Call integrity

`reconnect-storm` tags every `tools/call` with a call id in `params._meta["io.mcpload/callId"]` (`<prefix>-<vu>-<n>`; mcpload passes a fresh `CALL_ID_PREFIX` per run). With `--calls-url`, mcpload asks the server which of this run's ids it executed and how often, and compares that with what the client saw. The result goes in `report.json` `callIntegrity` and the `call_integrity` verdict:

| | `call_integrity` |
|---|---|
| a call id ran more than once on the server (e.g. `RETRY_ON_ERROR=1` re-sent a call whose response was lost in the restart, or `DOUBLE_SEND` against a non-atomic duplicate check) | `fail`, naming how many were re-sent by the client and how many were not |
| calls failed on the client (error or timeout) but ran on the server | `warn`: the client can't tell them from calls that never ran |
| otherwise | `pass` |
| no call carried an id, or no `--calls-url` (or it could not be read) | `skipped`: "server-side executions not measured ..." |

The TS demo server implements the endpoint with `TRACK_CALLS=1` (see [demo-servers/README.md](../../demo-servers/README.md#call-tracking-and-chaos-restarts-ts-image-off-in-compose)); [scenarios/README.md](../../scenarios/README.md#reconnect-storm) describes how to add the same record to your own server.
