# Scenario library

These are k6 scripts for load- and soak-testing MCP servers. They need a k6 binary built with the `xk6-mcpload` extension. Stock k6 doesn't have `k6/x/mcpload`.

```sh
cd xk6-mcpload && xk6 build --with github.com/atul121001/mcpload/xk6-mcpload=. --output ../k6 && cd ..
```

On Windows, use `k6.exe` below. For a soak run, use the `mcpload` CLI rather than calling `k6 run` directly. The CLI wraps these scripts and adds server sampling, verdicts and `report.json`/`report.html`.

| Script | What it does | Default load |
|---|---|---|
| `agent-session.js` | Each VU loops through agent sessions: connect, `tools/list`, 1–5 rounds of `callParallel` with think time, then close. | 10 VUs for 2m |
| `agent-workflow.js` | Each VU loops through a multi-step agent plan (`WORKFLOW`): parallel fan-out, a think pause, then calls whose arguments come from earlier results (optionally one call per result), up to a final call. Measures each step's wall time and the end-to-end workflow time. See [Agent workflows](#agent-workflows). | 10 VUs for 2m |
| `burst.js` | Two phases. `init_flood` runs bare connect/close at 100/s to test the initialize storm. `agents` then ramps VUs from 0 to 200 in 10s. | about 1.5 min |
| `soak.js` | Agent sessions at a constant arrival rate (`RATE` per `TIME_UNIT`; fractional rates work). A warm-up ramp comes first and a zero-load cool-down comes last. | 3m warm-up + 30m load + 5m cool-down |
| `lb-check.js` | A stateful flow with sequential calls. Fails on any `session_not_found` or `header_mismatch`. | 10 VUs for 1m |
| `isolation.js` | Do fast tools wait behind slow ones? Runs agent sessions twice at the same concurrency: `solo` (the mix without `SLOW_TOOLS`), then `mixed` (the full mix). mcpload compares each tool's p95 between the two (verdict `tool_isolation`). | 20 VUs, 1m per phase |
| `oauth-refresh.js` | 50 VUs on short-lived client-credentials tokens. Measures `mcp_oauth_refresh_duration` and counts auth errors. | 3m |

## Demo targets (`demo-servers/`, `docker compose up -d --build`)

| Port | Target | Try |
|---|---|---|
| 3001 | ts-healthy | `./k6 run -e MCP_URL=http://localhost:3001/mcp scenarios/agent-session.js` |
| 3002 | ts-leaky | `./k6 run -e MCP_URL=http://localhost:3002/mcp -e SOAK_MIN=30 scenarios/soak.js` |
| 3003 | py-healthy (stateless) | `./k6 run -e MCP_URL=http://localhost:3003/mcp scenarios/burst.js` |
| 3004 | lb-stateful (2 replicas, no sticky sessions) | `./k6 run -e MCP_URL=http://localhost:3004/mcp scenarios/lb-check.js` (expected to fail) |
| 3005 | stateless-2026 (2 replicas) | `./k6 run -e MCP_URL=http://localhost:3005/mcp -e MCP_PROTOCOL=2026-07-28 scenarios/lb-check.js` (expected to pass) |
| 3008 | ts-pooled (all tools share 2 slots) | `./mcpload run --url http://localhost:3008/mcp --scenario isolation` (expected to fail `tool_isolation`; ts-healthy on 3001 passes) |
| 3006 / 3007 | mock-oauth / ts-oauth | `./k6 run -e MCP_URL=http://localhost:3007/mcp -e OAUTH_TOKEN_URL=http://localhost:3006/token -e OAUTH_CLIENT_ID=mcpload -e OAUTH_CLIENT_SECRET=secret scenarios/oauth-refresh.js` |

Every demo server exposes the tools `fast`, `slow`, `flaky`, `big` and `search`.

## Which tools get called

Each session lists the server's tools, then chooses from them like this:

- **`TOOL_MIX` set**: only the tools it names, picked by weight. If a name isn't on the server's list, the session skips it and warns once per VU, showing the tools the server does list. If **no** name matches (a typo, or the wrong server), the session records a failed `tool mix matched` check, logs a warning and sleeps `CONNECT_BACKOFF_MS` rather than hot-looping. The `checks` threshold then fails the run with exit code 99. `lb-check.js` does the same.
- **`TOOL_MIX` unset, demo server**: the demo mix `{"search":5,"fast":3,"slow":1,"big":1,"flaky":1}`, called with the demo arguments. A server only counts as a demo server if it lists **all five** demo tools. A real server that happens to have a tool named `search` isn't treated as one.
- **`TOOL_MIX` unset, any other server**: every listed tool, each with the same weight.

Arguments come from `TOOL_ARGS[name]` when it's set. Otherwise demo servers get the demo arguments, and every other tool gets placeholder values built from its `inputSchema`. Placeholders are only generated for required properties, but they recurse into nested objects. They respect:

- `const`, `default`, `enum` and `examples[0]`
- the first non-null `anyOf`/`oneOf` branch, and `allOf`
- arrays with `minItems`, with items built from `items` or `prefixItems`
- string `format` (`uri`, `email`, `uuid`, `date`, `date-time`, …), `minLength` and `maxLength`
- number `minimum`, `maximum`, `exclusiveMinimum`, `exclusiveMaximum` and `multipleOf`
- local `$ref` (`#/$defs/…`, `#/definitions/…`)

If a tool needs meaningful values, such as a real ID, set them in `TOOL_ARGS`.

> [!WARNING]
> If `TOOL_MIX` is unset and the server isn't a demo server, the run calls **every** listed tool, many times, with placeholder arguments. If the server has tools that write, delete, send or spend money (`delete_user`, `send_email`, `create_payment`, …), restrict `TOOL_MIX` to read-only tools or run against a disposable environment.

## Configuration (env vars, `-e NAME=value`)

| Var | Default | Meaning |
|---|---|---|
| `MCP_URL` | `http://localhost:3001/mcp` | target endpoint |
| `MCP_PROTOCOL` | `auto` | `auto`, `2026-07-28`, `2025-06-18`, … |
| `MCP_TOKEN` | – | static bearer token |
| `OAUTH_TOKEN_URL`, `OAUTH_CLIENT_ID`, `OAUTH_CLIENT_SECRET` | – | client-credentials OAuth; takes precedence over `MCP_TOKEN` |
| `MCP_HEADERS` | `{}` | extra headers as JSON |
| `MCP_TIMEOUT` | `30s` | per-request timeout |
| `INCLUDE_PAYLOADS` | off | `1` keeps tool args and results |
| `TOOL_MIX` | demo mix on demo servers, otherwise uniform | tool weights as JSON. See [Which tools get called](#which-tools-get-called). |
| `TOOL_ARGS` | `{}` (demo args on demo servers) | per-tool arguments as JSON. Tools not listed here get placeholder values derived from their `inputSchema`. |
| `THINK_MS` | `500` | mean think time between rounds (exponential, capped at 5×) |
| `PARALLEL` | `3` | concurrent `tools/call` per round (`callParallel`) |
| `ROUNDS` | `1-5` | rounds per session, as `N` or `MIN-MAX` |
| `P95_MS`, `P99_MS`, `ERR_RATE` | `800`, `2000`, `0.01` | default per-tool budgets. They are also the catch-all budget for any tool without a budget of its own. |
| `TOOL_BUDGETS` | `{}` | per-tool overrides, e.g. `{"slow":{"p95":1500,"p99":2500}}`. These are merged over the built-in `{"flaky":{"errRate":0.2}}`, which exists for the demo `flaky` tool (it fails 10% of calls by design). |
| `CONNECT_P95_MS` | `1500` | `mcp_connect_duration` p95 budget |
| `CHECKS_MIN` | `0.99` | minimum pass rate over all checks (`connect ok`, `tools/list returned tools`, `tool mix matched`, `tools/call no transport error`, …) |
| `CONNECT_BACKOFF_MS` | `1000` | how long to sleep after a failed `connect()`, or when `TOOL_MIX` matches no listed tool, so the target isn't hot-looped |
| `SAMPLING`, `ELICITATION`, `ROOTS` | off | answer the server's `sampling/createMessage`, `elicitation/create` or `roots/list` requests (stateful servers) and declare the capability: `1` for the default mock answer, or the client option as JSON, e.g. `SAMPLING={"delayMs":200}`, `ELICITATION={"action":"decline"}`. See [xk6-mcpload](../xk6-mcpload/README.md#server-to-client-requests). The demo mix never calls the TS demo tools that need them, so name them in `TOOL_MIX`: `--env SAMPLING=1 --env ELICITATION=1 --env TOOL_MIX={"sample_llm":1,"elicit_input":1,"fast":1}` |

Each script also has its own knobs, documented in its header comment. Examples: `VUS`, `DURATION`, `BURST_VUS`, `FLOOD_RATE`, `SOAK_MIN`, `WARMUP_MIN`, `COOLDOWN_MIN`, `RATE`, `TIME_UNIT`, `PRE_VUS`, `MAX_VUS`, `STEPS`, `REFRESH_P95_MS`.

## Thresholds

k6 fixes thresholds before the script contacts the server, so `lib/config.js#buildThresholds()` can't know the server's tool names in advance. It builds this set:

```js
// one pair per tool: every explicit TOOL_MIX name (the five demo names when TOOL_MIX is unset) and every TOOL_BUDGETS name
'mcp_req_duration{tool:<name>}':       ['p(95)<P95', 'p(99)<P99'],   // TOOL_BUDGETS[name] overrides
'mcp_tool_error_rate{tool:<name>}':    ['rate<ERR_RATE'],
// catch-all for calls to any tool that has no threshold of its own
'mcp_req_duration{budget:default}':    ['p(95)<P95_MS', 'p(99)<P99_MS'],
'mcp_tool_error_rate{budget:default}': ['rate<ERR_RATE'],
'mcp_connect_duration':                ['p(95)<CONNECT_P95_MS'],
'checks':                              ['rate>=CHECKS_MIN'],
```

Every tool that gets called has a budget. A tool has its own threshold if it's named in an explicit `TOOL_MIX`, named in `TOOL_BUDGETS`, or is a demo tool on a demo server. Calls to any other tool fall under the catch-all, for example `get_weather` on your server when `TOOL_MIX` is unset. Those calls run in their own `callParallel` group with the VU tag `budget=default` (set through `exec.vu.metrics.tags`), which is what the catch-all thresholds match on. So a round can send up to two concurrent groups instead of one. The demo `flaky` tool keeps its 0.2 error budget and is never counted in the catch-all. To give one of your own tools a looser budget, add it to `TOOL_BUDGETS`.

The `checks` threshold is what fails a run where no session ever starts (every `connect()` gets a 401 or is refused) or where `TOOL_MIX` matches nothing. In those runs the tool thresholds get no samples, and k6 passes a threshold that has no samples.

Each scenario adds its own thresholds on top:

- `lb-check` adds `mcp_errors{error_type:session_not_found}: count<1`, `mcp_errors{error_type:header_mismatch}: count<1` and `checks{check:lb request ok}: rate>=LB_MIN_OK`. The last one requires every request to succeed whichever replica serves it, which is the meaningful check for stateless servers since they have no session to lose.
- `isolation` adds `checks{check:slow tools in mix}: rate>0.999`, so a `SLOW_TOOLS` name that isn't in the tool mix fails the run instead of comparing two identical phases. `SLOW_TOOLS` (comma-separated, default `slow`) names your slow tools; `DURATION` is per phase.
- `oauth-refresh` adds a p95 budget on `mcp_oauth_refresh_duration`.
- `agent-workflow` adds per-tool thresholds for every tool its plan calls, plus `mcp_workflow_duration: p(95)<WORKFLOW_P95_MS` (and `p(99)<WORKFLOW_P99_MS` when set), `mcp_workflow_complete: rate>=WORKFLOW_MIN_COMPLETE`, and `mcp_workflow_step_duration{step:<name>}: p(95)<STEP_P95_MS` for each step (`STEP_BUDGETS` overrides per step). See [Agent workflows](#agent-workflows).

If any threshold fails, k6 exits with code 99, which gates CI.

## Agent workflows

`agent-workflow.js` models an agent working through a plan: it fires independent tool calls together, pauses to reason, then makes calls that depend on what came back. Each step is one parallel batch (`callParallel`), so a step takes as long as its slowest call. That is the latency an agent actually waits for.

```sh
./mcpload run --url http://localhost:3001/mcp --scenario agent-workflow
./mcpload run --url http://localhost:3008/mcp --scenario agent-workflow --env STEP_P95_MS=100 \
  --env 'STEP_BUDGETS={"inspect":{"p95":300}}'   # ts-pooled: steps queue behind `slow`, expected FAIL
```

The plan is the `WORKFLOW` env var: JSON, either `{"steps": [...]}` or the steps array itself. The full format is documented at the top of `lib/workflow.js`.

```jsonc
{ "steps": [
  { "name": "gather", "calls": [                                   // step 1: parallel fan-out
    { "tool": "search", "args": { "query": "overdue invoices", "limit": 5 }, "as": "invoices" },
    { "tool": "search", "args": { "query": "customer accounts", "limit": 3 }, "as": "accounts" },
    { "tool": "fast" } ] },
  { "name": "inspect", "calls": [                                  // step 2: one call per step-1 result
    { "tool": "search", "forEach": { "$from": "invoices", "path": "results", "max": 3 },
      "args": { "query": { "$from": "$item", "path": "title" }, "limit": 2 }, "as": "details" },
    { "tool": "slow", "args": { "ms": 200 } } ] },
  { "name": "act", "calls": [                                      // step 3: args from step 2
    { "tool": "search", "args": { "query": { "$from": "details", "path": "0.results.0.title" }, "limit": 1 }, "as": "record" },
    { "tool": "big" }, { "tool": "flaky" } ] },
  { "name": "report", "calls": [                                   // step 4: an id pulled out with a regex
    { "tool": "search", "args": { "query": { "$from": "record", "path": "results.0.url", "match": "/search/([^/]+)/" }, "limit": 1 } } ] } ] }
```

That is the default plan. It works on every demo server: 4 steps, 11 calls and 3 think pauses per workflow.

- **Step**: `{name?, thinkMs?, calls}`. The name (`[A-Za-z0-9_.-]`, default `stepN`) tags the step's metrics. Before every step except the first, the VU pauses for an exponential time with mean `thinkMs`, or `THINK_MS` when the step doesn't set it.
- **Call**: `{tool, args?, as?, repeat?, forEach?}`. `args` are shallow-merged over the tool's usual args: `TOOL_ARGS`, else the demo args on a demo server, else placeholders from its `inputSchema`. `repeat: N` sends N identical calls in the step. `forEach` sends one call per element of a list from an earlier result, up to `max` (default 5). `as` names the result for later steps. With `repeat` or `forEach`, the name holds the list of successful results.
- **Reference**: any arg value (at any depth) can be `{"$from": "<as name>", "path"?, "match"?}`. The named result is read as its `structuredContent` when present, else as its first text content parsed as JSON, else as that text. `path` walks into it (`results.0.title` or `results[0].title`). `match` is a regex whose first capture group (or whole match) becomes the value. Inside a `forEach` call, `$from: "$item"` is the current element. A reference can only use names from **earlier** steps, because calls within a step run in parallel. The plan is checked when k6 starts, and a bad plan stops the run with the location of the problem.

When the plan doesn't fit the server, the scenario handles it like an unknown `TOOL_MIX` name. If the server doesn't list a tool the plan needs, the VU warns once (listing the missing tools and the server's tools), records a failed `workflow tools listed` check and sleeps `CONNECT_BACKOFF_MS`. If a reference path is missing from a *successful* result, it records a failed `workflow dependency resolved` check and warns once. If a reference points at a call that *failed* (for example `isError`), the workflow just ends early, the way an agent would stop. In all of these cases the workflow counts as not complete.

| Metric | Type | Meaning |
|---|---|---|
| `mcp_workflow_step_duration{step}` | Trend (ms) | wall time of one step's batch |
| `mcp_workflow_duration` | Trend (ms) | one complete workflow, from `connect()` to the end of the last step, think pauses included |
| `mcp_workflow_complete` | Rate | workflows that ran every step |

mcpload writes them to `report.json` as `workflow` (per-step and end-to-end p50/p95/p99/max, plus the completion rate), and the HTML report shows them in a "Workflow steps" table.

| Var | Default | Meaning |
|---|---|---|
| `WORKFLOW` | the plan above | the plan, as JSON |
| `WORKFLOW_P95_MS` | `5000` | p95 budget for `mcp_workflow_duration`. It includes think time: with the default plan and `THINK_MS=500`, a healthy server's p95 is about 3 s. |
| `WORKFLOW_P99_MS` | unset | optional p99 budget for `mcp_workflow_duration` |
| `STEP_P95_MS`, `STEP_P99_MS` | `1000`, unset | budget for every step's `mcp_workflow_step_duration{step:<name>}` |
| `STEP_BUDGETS` | `{}` | per-step overrides, e.g. `{"inspect":{"p95":300,"p99":500}}` |
| `WORKFLOW_MIN_COMPLETE` | `0.95` | minimum `mcp_workflow_complete` rate |

## Soak phases

`soak.js` has three phases:
- warm-up: `[0, W)`, with `W = WARMUP_MIN*60` (default is 10% of `SOAK_MIN`, at least 1 minute);
- load: `[W, W + SOAK_MIN*60)`;
- cool-down: the `COOLDOWN_MIN*60` seconds after load (default 5 minutes).

During cool-down a single idle VU sleeps, so there is no MCP traffic. Sessions still in flight may finish up to 30 s into cool-down. The CLI computes `phases` in `report.json` from the same env vars.

The load is `RATE` sessions per `TIME_UNIT` (defaults: `2` per `1s`). k6's arrival-rate executors only accept an integer rate, so a fractional rate is converted to the smallest unit out of `1s`, `1m` and `1h` that makes it a whole number. For example, `RATE=0.05` runs as 3 sessions per `1m` and `RATE=0.5` as 30 per `1m`. Rates below one per hour are rounded to a whole number per hour. `PRE_VUS` defaults to about 15 s worth of arrivals, clamped to 2–20, and `MAX_VUS` defaults to 200. The rate has no effect on how long each phase lasts.

For leak detection, run at least 10 minutes of load (`SOAK_MIN>=10`; the default is 30). In shorter runs there are too few samples for a reliable memory slope, and warm-up effects such as caches, JIT and connection pools dominate.

## Syntax check and unit tests

`package.json` sets `"type": "module"`, so Node can parse these files. It doesn't run them.

```sh
for f in scenarios/lib/*.js scenarios/*.js; do node --check "$f"; done
```

The pure helpers (schema placeholders, tool selection, budget coverage, fractional rates, workflow plans) have unit tests that need only Node, not k6:

```sh
node scenarios/lib/schema-args.test.mjs
node scenarios/lib/workflow.test.mjs
```
