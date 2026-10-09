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
| `workload.js` | A workload profile (`mcpload run --workload <file.yaml>`): each VU iteration is one agent session that picks a business flow by weight, takes a row from each test data pool and runs the flow's steps. Measures every flow's end-to-end time, completion rate and step times against the flow's own budgets. See [Workload profiles](#workload-profiles). | the profile's `agents` (10) for its `duration` (2m) |
| `burst.js` | Two phases. `init_flood` runs bare connect/close at 100/s to test the initialize storm. `agents` then ramps VUs from 0 to 200 in 10s. | about 1.5 min |
| `soak.js` | Agent sessions at a constant arrival rate (`RATE` per `TIME_UNIT`; fractional rates work). A warm-up ramp comes first and a zero-load cool-down comes last. | 3m warm-up + 30m load + 5m cool-down |
| `lb-check.js` | A stateful flow with sequential calls. Fails on any `session_not_found` or `header_mismatch`. | 10 VUs for 1m |
| `isolation.js` | Do fast tools wait behind slow ones? Runs agent sessions twice at the same concurrency: `solo` (the mix without `SLOW_TOOLS`), then `mixed` (the full mix). mcpload compares each tool's p95 between the two (verdict `tool_isolation`). | 20 VUs, 1m per phase |
| `step-load.js` | How many agents at once before the server breaks its budgets? Agent sessions with concurrency rising in steps (`STEPS`); each step ramps for `RAMP`, then holds for `STEP_DURATION`. mcpload judges every step against the per-tool budgets (verdict `capacity`). See [Step load](#step-load). | steps 10, 25, 50, 100, 200 VUs, 5s ramp + 1m hold each (about 5.5 min) |
| `version-skew.js` | Replicas on different builds behind one LB (a rolling deploy halfway through). Agent sessions negotiate (`auto`), list tools and make sequential calls; every failed request is classified as a fast typed error or a hang. mcpload turns it into the `version_skew` verdict. See [Version skew](#version-skew). | 10 VUs for 1m |
| `long-lived.js` | Each VU opens **one** session and keeps it until the end of the load phase, calling tools with think time and pinging when quiet; it reconnects only when the session dies. mcpload checks that no session dies early and compares late with early calls of the same sessions (verdict `session_survival`). Has soak-style phases, so the leak verdicts run too. See [Long-lived sessions](#long-lived-sessions). | 20 VUs: 1m warm-up + 10m sessions + 2m cool-down |
| `reconnect-storm.js` | Agents that reconnect as soon as their session breaks. Use it with `mcpload run --chaos-restart`: mcpload restarts the server's container mid-run and measures how long until errors and connect times are back to normal (verdict `recovery`). Every call carries a call id, so with `--calls-url` mcpload also counts calls that failed on the client but ran on the server, and calls that ran twice (verdict `call_integrity`). See [Reconnect storm](#reconnect-storm). | 20 VUs for 2m |
| `oauth-refresh.js` | 50 VUs on short-lived client-credentials tokens. Measures `mcp_oauth_refresh_duration` and counts auth errors. | 3m |

## Demo targets (`demo-servers/`, `docker compose up -d --build`)

| Port | Target | Try |
|---|---|---|
| 3001 | ts-healthy | `./k6 run -e MCP_URL=http://localhost:3001/mcp scenarios/agent-session.js` |
| 3002 | ts-leaky | `./k6 run -e MCP_URL=http://localhost:3002/mcp -e SOAK_MIN=30 scenarios/soak.js` |
| 3003 | py-healthy (stateless) | `./k6 run -e MCP_URL=http://localhost:3003/mcp scenarios/burst.js` |
| 3004 | lb-stateful (2 replicas, no sticky sessions) | `./k6 run -e MCP_URL=http://localhost:3004/mcp scenarios/lb-check.js` (expected to fail) |
| 3005 | stateless-2026 (2 replicas) | `./k6 run -e MCP_URL=http://localhost:3005/mcp -e MCP_PROTOCOL=2026-07-28 scenarios/lb-check.js` (expected to pass) |
| 3008 | ts-pooled (all tools share 2 slots) | `./mcpload run --url http://localhost:3008/mcp --scenario isolation` (expected to fail `tool_isolation`; ts-healthy on 3001 passes). `./mcpload capacity --url http://localhost:3008/mcp --from 5 --to 80 --step-duration 20s` breaks at 20 agents (estimate ~13). |
| 3009 | skew (old TS build + new Go build, typed errors) | `./mcpload run --url http://localhost:3009/mcp --scenario version-skew` (expected to warn `version_skew`) |
| 3010 | skew-hang (old build never answers what it can't serve) | `./mcpload run --url http://localhost:3010/mcp --scenario version-skew --env MCP_TIMEOUT=5s` (expected to fail `version_skew`) |
| 3006 / 3007 | mock-oauth / ts-oauth | `./k6 run -e MCP_URL=http://localhost:3007/mcp -e OAUTH_TOKEN_URL=http://localhost:3006/token -e OAUTH_CLIENT_ID=mcpload -e OAUTH_CLIENT_SECRET=secret scenarios/oauth-refresh.js` |

Every demo server exposes the tools `fast`, `slow`, `flaky`, `big` and `search`. The new build of the skew targets (`skew-new`) also lists `new_tool`.

## stdio targets

*v0.6.0, unreleased.* With `MCP_COMMAND` set, `lib/config.js` gives `new mcp.Client(...)` `command`, `args`, `env` and `cwd` instead of `url`, and drops `MCP_HEADERS`, `MCP_TOKEN` and OAuth (HTTP-only). Start-up lines print `stdio: <command line>` (never the env values) instead of the URL. Each session is one server process, so `connect()` includes starting it. `lb-check.js`, `version-skew.js` and `oauth-refresh.js` stop at init with an error: they need an HTTP target. A session whose process exited (error type `process_exit`) counts as broken in `long-lived.js` and `reconnect-storm.js`, like `session_not_found`.

```sh
./k6 run -e 'MCP_COMMAND=["node","demo-servers/ts-server/server.mjs","--stdio"]' scenarios/agent-session.js
./k6 run -e 'MCP_COMMAND=["node","demo-servers/ts-server/server.mjs","--stdio"]' -e 'MCP_COMMAND_ENV={"PERSONA":"blocking"}' scenarios/isolation.js
```

With the CLI: `mcpload run --command "node server.mjs --stdio" [--command-env K=V] [--command-cwd DIR]`. Guide: [docs/guide/stdio.md](../docs/guide/stdio.md).

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
| `MCP_COMMAND` | – | stdio target instead of `MCP_URL` (v0.6.0, unreleased): JSON array of the program and its arguments, e.g. `["node","server.mjs","--stdio"]`. Each session starts its own process. Set by `mcpload run --command`. See [stdio targets](#stdio-targets) |
| `MCP_COMMAND_ENV` | – | JSON object of strings added to the server process's environment (values are never logged) |
| `MCP_COMMAND_CWD` | – | working folder of the server process |
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
| `CANCEL_RATE`, `CANCEL_AFTER_MS`, `CANCEL_TOOLS`, `CANCEL_WAIT` | `0`, `150`, all tools, `2s` | cancellation testing in every scenario built on `agentSession` / `callTools`: each `tools/call` to a `CANCEL_TOOLS` tool (comma-separated) is cancelled with probability `CANCEL_RATE` if it has no response `CANCEL_AFTER_MS` after it was sent (`cancelAfterMs`; a `notifications/cancelled` on stateful sessions, a closed stream on 2026-07-28). `CANCEL_WAIT` is how long the stream is still read for a late response (client option `cancelWait`). Cancelled calls are not errors; mcpload adds the `cancellation` verdict, e.g. `--env CANCEL_RATE=0.3 --env CANCEL_TOOLS=slow --sampler prometheus --prom-url http://localhost:3011/metrics` |
| `RESOURCE_READ_RATIO`, `PROMPT_GET_RATIO` | `0`, `0` | mix `resources/read` and `prompts/get` into every scenario built on `agentSession` (agent-session, burst, soak, step-load, isolation, oauth-refresh). Each is the share (0..1, together at most 1) of a round's `PARALLEL` calls that read a resource or get a prompt instead of calling a tool. When either is set, each session lists resources and resource templates (for reads) and prompts (for gets) once after `tools/list`, then picks uniformly from them: a template's variables get a random id (`demo://items/{id}` → `demo://items/417`), a prompt's required arguments get short strings. If the server lists nothing (or rejects the list), those steps stay tool calls and the VU warns once. Reads and gets are tagged `method:resources/read` / `method:prompts/get` with a `resource` / `prompt` tag, fall under the catch-all `budget:default` latency thresholds, and are not counted in `mcp_tool_error_rate`. At `0` sessions behave exactly as before. Example: `--env RESOURCE_READ_RATIO=0.3 --env PROMPT_GET_RATIO=0.2` (the TS demo servers list two resources, one template and one prompt) |

Each script also has its own knobs, documented in its header comment. Examples: `VUS`, `DURATION`, `BURST_VUS`, `FLOOD_RATE`, `SOAK_MIN`, `SESSION_MIN`, `WARMUP_MIN`, `COOLDOWN_MIN`, `RATE`, `TIME_UNIT`, `PRE_VUS`, `MAX_VUS`, `STEPS`, `REFRESH_P95_MS`.

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
- `workload` adds per-tool thresholds for every tool the profile calls, plus, per flow, `mcp_workload_flow_duration{flow:<name>}: p(95)<…` (and `p(99)` when budgeted), `mcp_workload_flow_complete{flow:<name>}: rate>=…`, and `mcp_workload_step_duration{flow:<name>,flow_step:<step>}` for each step with a budget. The numbers come from the profile's `budgets`. See [Workload profiles](#workload-profiles).
- `version-skew` replaces the default set with `mcp_skew_failures{kind:hang}: count<1` (any hang fails the run) and `mcp_skew_ok: rate>0` (fails a run where no request succeeded at all, e.g. a wrong URL). It has no latency or error budgets: failures are expected under skew, and mcpload judges how they fail (verdict `version_skew`, a warning when all failed fast).
- `step-load` sets **no** thresholds. Its budgets are judged per step by mcpload (see below), because a breach at the top step is what a step-load run is looking for, not a failed run.
- `long-lived` adds `mcp_session_survived: rate>=SURVIVAL_MIN` (default 1: any session that dies early fails the run).
- `reconnect-storm` sets **no** thresholds: errors during the outage are what the test provokes. The `recovery` verdict judges the run.

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

## Workload profiles

`agent-workflow` runs one plan over and over. Real traffic is a mix: most agents look something up, fewer change something. A workload profile describes that mix in one YAML (or JSON) file: named flows with weights, each a list of steps in the agent-workflow format, test data to fill in, and a budget per flow.

```sh
./mcpload run --url http://localhost:3001/mcp --workload examples/workloads/customer-support.yaml   # PASS
./mcpload run --url http://localhost:3008/mcp --workload examples/workloads/customer-support.yaml   # ts-pooled: FAIL
```

`--workload` picks `scenarios/workload.js` (unless you pass `--scenario`). mcpload checks the whole file before k6 starts and stops with the flow, step and field at fault, e.g. ``flow `lookup-orders` step 2 (get_orders, tool search) args.query: 'custmer' is not the `as` name of a call in an earlier step (known: customer)``. It reads the CSV files (relative to the profile) and hands k6 a normalized JSON copy in `WORKLOAD_FILE` (format at the top of `lib/workload.js`). `--env WORKFLOW=...` and the `agent-workflow` scenario are unchanged.

```yaml
workload:
  name: customer-support
  agents: 20            # VUs (--vus wins; default 10)
  duration: 1m          # (--duration wins; default 2m)
  thinkMs: 300          # mean pause between steps (a flow or step can set its own; default THINK_MS)
  data:
    customers: { file: customers.csv, pick: random }   # or pick: sequential; or values: [...]
  budgets: { p95: 2s, completion: 99% }               # defaults for every flow
  flows:
    - name: lookup-orders
      weight: 60
      steps:
        - name: search_customer
          tool: search
          args: { query: "customer {{data.customers.email}}", limit: 1 }
          as: customer
        - name: get_orders
          tool: search
          args: { query: { $from: customer, path: results.0.title }, limit: 3 }
          as: orders
        - name: get_order_details
          parallel:                                 # calls sent together (callParallel)
            - { tool: search, forEach: { $from: orders, path: results, max: 3 }, args: { query: { $from: $item, path: title } } }
            - big
    - name: create-ticket
      weight: 10
      budgets: { p95: 3s, completion: 95%, steps: { create_ticket: { p95: 1s } } }
      steps:
        - { name: search_customer, tool: search, args: { query: "customer {{data.customers.email}}", limit: 1 } }
        - { name: create_ticket, tool: slow, args: { ms: 700 } }
```

(Abridged from `examples/workloads/customer-support.yaml`, which also has a `check-subscription` flow and a `notify_customer` step.)

- **Flow**: `{name, weight, thinkMs?, budgets?, steps}`. Each agent session (one VU iteration) picks a flow with probability `weight / sum of weights`, so 60/30/10 gives 60% of sessions to the first flow.
- **Step**: a bare tool name (called with `TOOL_ARGS`, the demo args or schema placeholders, like other scenarios), a single call `{name?, thinkMs?, tool, args?, as?, repeat?, forEach?}`, or a parallel batch `{name?, thinkMs?, parallel: [call, ...]}` where each call is a tool name or `{tool, args?, as?, repeat?, forEach?}`. Calls, `as`, `$from`/`path`/`match`, `forEach` and `repeat` work exactly as in [Agent workflows](#agent-workflows). A step is named after its tool unless it has a `name` (`step<N>` for a parallel batch; repeats get `-2`, `-3`).
- **Data**: `data.<pool>` is `{file: x.csv}` (header row = column names) or `{values: [...]}` (plain values or mappings), with `pick: random` (default) or `sequential` (rows in order across all agents). Each session takes one row per pool. In args, `"{{data.customers.email}}"` (or `"{{data.regions}}"` for plain values) alone in a string becomes the value itself; inside a longer string it is inserted as text. CSV values are text.
- **Budgets**: `p95`, `p99` (end to end, think time included; `800ms`, `3s` or a number of ms), `completion` (`0.99` or `99%`), `stepP95`/`stepP99` (every step) and, in a flow, `steps: {<step>: {p95, p99}}`. Top-level `budgets` are defaults; a flow's own win. Defaults: p95 5 s, completion 95%, no step budgets. Per-tool budgets still come from `P95_MS`/`P99_MS`/`ERR_RATE`/`TOOL_BUDGETS`.

Unlike agent-workflow, **a failed call ends the flow**: a transport error or `isError: true` means the business task did not get done. A flow is complete when every step ran and every call succeeded. A server that doesn't list a flow's tools gets a failed `workload tools listed` check, as in agent-workflow.

| Metric | Type | Meaning |
|---|---|---|
| `mcp_workload_flow_duration{flow}` | Trend (ms) | one complete flow, from `connect()` to the end of its last step, think pauses included |
| `mcp_workload_flow_complete{flow}` | Rate | flows that completed |
| `mcp_workload_step_duration{flow,flow_step}` | Trend (ms) | wall time of one step's batch. The tag is `flow_step`, not `step`, because `step-load` tags every request with `step`. |

Every request made during a session (connect, `tools/list`, each `tools/call`) also carries the `flow` tag, so `mcp_req_duration{flow:create-ticket,tool:slow}` works in your own thresholds. mcpload prints a per-flow table (weight, runs, completed %, p50/p95/p99 end to end, slowest step), writes `report.json` `workload` and adds the `workload` verdict, which names each flow that broke a budget in plain words: ``flow `create-ticket` p95 4.1 s > 3 s budget; 92% completed (< 99%)``. The HTML report has a "Workload: <name>" table with over-budget cells highlighted.

`examples/workloads/` has the demo profile above (its comments map the business names onto the demo tools), its `customers.csv`, and `template.yaml`, a commented starting point for your own server. Other scenarios can run the same sessions with `import { runWorkloadSession } from './workload.js'` (it needs `WORKLOAD_FILE` too).

## Version skew

During a rolling deploy, replicas on the old and the new build sit behind the same load balancer, so one client's requests reach both. That is harmless when both builds speak the same protocol revision and list the same tools. When they don't (for example a rollout from the stateful protocol to the stateless 2026-07-28 one, or a build that adds a tool), what matters is how the mismatched requests fail: a typed error that comes back at once can be caught and retried, while a request that is never answered hangs every client until its timeout, for as long as the deploy lasts.

`version-skew.js` runs agent sessions against the LB. Each session connects with `MCP_PROTOCOL` (default `auto`, so every session goes through negotiation, including the retry after an `Unsupported protocol version` error), lists tools and makes `STEPS` sequential calls, each its own trip through the LB, then closes. Every failed request is classified (`lib/skew.js`):

| Kind | Meaning |
|---|---|
| `fast` | a typed error (HTTP 4xx/5xx, a JSON-RPC error code such as -32022 Unsupported protocol version or -32601 Method not found, an "unknown tool" error) returned in under `FAIL_FAST_MS` |
| `hang` | error type `timeout` (no answer within `MCP_TIMEOUT`), or a failure that took `HANG_MS` or longer |
| `slow` | anything else: a typed error slower than `FAIL_FAST_MS`, or a failure without an HTTP response |

Ordinary tool errors (`isError` results such as the demo `flaky` tool) are not failures. A session ends at its first hang, as an agent would give up. The `reason` tag says what went wrong: `unsupported_protocol`, `method_not_found`, `unknown_tool`, `session_not_found`, `header_mismatch`, `auth`, `http_<status>`, `jsonrpc_error`, `timeout` or `transport`.

With `TOOLS_CACHE_TTL` set (e.g. `5m`), a VU reuses the `tools/list` result of an earlier session for that long instead of listing again, like a client that honours a list's cache TTL. It may then call a tool the replica serving the call doesn't have (`unknown_tool`). The tool mix is uniform over every listed tool unless `TOOL_MIX` is set.

Which replica answered comes from a response header (`SERVED_BY_HEADER`, default `X-Served-By`; the demo LBs also set `X-Upstream`), through the client's `servedBy` fields. Requests that got no response are counted as `no answer`.

| Metric | Type | Meaning |
|---|---|---|
| `mcp_skew_requests{op, outcome, replica, protocol}` | Counter | every request (`op`: connect, list, call, close) |
| `mcp_skew_failures{op, kind, reason}` | Counter | failed requests |
| `mcp_skew_failure_duration{op, kind, reason}` | Trend (ms) | time until the failure surfaced |
| `mcp_skew_negotiated{protocol, replica}` | Counter | successful connects by negotiated protocol and the replica that answered the handshake |
| `mcp_skew_ok` | Rate | requests that did not fail |

| Var | Default | Meaning |
|---|---|---|
| `VUS`, `DURATION` | `10`, `1m` | load |
| `STEPS` | `6` | sequential `tools/call` per session |
| `TOOLS_CACHE_TTL` | off | reuse a VU's earlier `tools/list` for this long (`5m`, `30s`, `500ms`) |
| `FAIL_FAST_MS` | `2000` | a typed error faster than this is `fast` |
| `HANG_MS` | `10000` | a failure this slow is a `hang` even with an answer. Timeouts always are; lower `MCP_TIMEOUT` (default 30s) to keep runs short. |
| `SERVED_BY_HEADER` | `X-Served-By` | response header that names the replica |
| `REMEMBER_PROTOCOL` | `0` | `1` reuses the protocol an earlier connect resolved (process-wide) instead of negotiating per session |

mcpload's verdict `version_skew` passes when nothing failed, warns when every failure was fast or slow (clients see errors they can handle), and fails when any request hung. The message reads like: "3540 of 7525 requests (47.0%) failed on a replica running a different build; 3540 failed fast with typed errors (good: clients can catch them): 3196 × HTTP 400, 344 × Unsupported protocol version (-32022)…". The demo targets on 3009 (warn) and 3010 (fail) show both outcomes, and a single build behind an LB (`stateless-2026` on 3005) passes.

Attribute failures with care: the scenario counts every failed request as a skew failure, so run it against a deployment that is otherwise healthy (`lb-check` first).

## Soak phases

`soak.js` has three phases:
- warm-up: `[0, W)`, with `W = WARMUP_MIN*60` (default is 10% of `SOAK_MIN`, at least 1 minute);
- load: `[W, W + SOAK_MIN*60)`;
- cool-down: the `COOLDOWN_MIN*60` seconds after load (default 5 minutes).

During cool-down a single idle VU sleeps, so there is no MCP traffic. Sessions still in flight may finish up to 30 s into cool-down. The CLI computes `phases` in `report.json` from the same env vars.

The load is `RATE` sessions per `TIME_UNIT` (defaults: `2` per `1s`). k6's arrival-rate executors only accept an integer rate, so a fractional rate is converted to the smallest unit out of `1s`, `1m` and `1h` that makes it a whole number. For example, `RATE=0.05` runs as 3 sessions per `1m` and `RATE=0.5` as 30 per `1m`. Rates below one per hour are rounded to a whole number per hour. `PRE_VUS` defaults to about 15 s worth of arrivals, clamped to 2–20, and `MAX_VUS` defaults to 200. The rate has no effect on how long each phase lasts.

For leak detection, run at least 10 minutes of load (`SOAK_MIN>=10`; the default is 30). In shorter runs there are too few samples for a reliable memory slope, and warm-up effects such as caches, JIT and connection pools dominate.

## Step load

The easiest way to run it is `mcpload capacity`, which sets `STEPS`, `STEP_DURATION` and `MIN_AGENTS` from flags and adds a capacity estimate and optional refinement steps:

```bash
./mcpload capacity --url http://localhost:3008/mcp --from 5 --to 80 --step-duration 20s --refine 2
```

`./mcpload run --scenario step-load --env STEPS=...` runs the same scenario and prints the same table. See [cmd/mcpload/README.md](../cmd/mcpload/README.md#capacity).

`step-load.js` runs agent sessions (as in `agent-session.js`) on one `ramping-vus` scenario named `steps`. Each step ramps for `RAMP` and then holds its level for `STEP_DURATION`. Every request made during a hold is tagged `step=<VUs>` (the tag is refreshed before each round of calls, so a session that crosses a step boundary is split correctly). Ramps are left untagged and are not judged.

| Var | Default | Meaning |
|---|---|---|
| `STEPS` | `10,25,50,100,200` | VU levels, strictly increasing |
| `START`, `STEP_FACTOR`, `MAX_VUS` | `10`, `2`, `200` | used instead of `STEPS` when `STEPS` is unset and any of them is set: `START`, `START×STEP_FACTOR`, … up to `MAX_VUS` |
| `STEP_DURATION` | `1m` | hold per step |
| `RAMP` | `5s` | ramp before each step |
| `ABORT_ERR_RATE` | `0.5` | stop the whole run (`exec.test.abort`, k6 exit code 108) once a VU sees more than this share of its calls fail within one step, after at least `ABORT_MIN_CALLS` (20) calls. A session that fails before its first call counts as one failed call. `0` turns the guard off. |
| `MIN_AGENTS` | – | read by mcpload (`--min-agents`, or `--target` with `mcpload capacity`): the concurrency the server must hold for the `capacity` verdict to pass |

mcpload judges each step against the same budgets the other scenarios turn into thresholds: per tool (with at least 10 calls in the step) p95 and p99 of successful calls against `P95_MS`/`P99_MS` and the error rate against `ERR_RATE`, with `TOOL_BUDGETS` overrides; connect p95 against `CONNECT_P95_MS` and the share of failed session starts against `ERR_RATE`. A step without any `tools/call` is a breach too. The first step with a breach is the breaking point and the step before it is the max sustainable concurrency; mcpload also estimates the capacity between the two and marks the first degraded and the first failed step. `ABORT_ERR_RATE` is also read by mcpload: a step whose error rate reaches it is marked `<- failure`. The latency and error drift verdicts are skipped in step-load runs, since the rising load is on purpose. Details: [cmd/mcpload/README.md](../cmd/mcpload/README.md#step-load-and-the-capacity-verdict).

Keep `STEP_DURATION` long enough for a few hundred calls per step (sessions last a few seconds). With very short steps, a tool with a 10% error rate, such as the demo `flaky`, can cross its 20% budget by chance.

## Long-lived sessions

A server can look healthy with short sessions while it drops sessions that stay open, or slows down as a session's state grows. In `long-lived.js` every VU is one agent (an IDE, a chat client) that opens a single session and holds it:

- warm-up `[0, W)`, `W = WARMUP_MIN*60`: the VUs open their sessions, spread evenly over the warm-up;
- load `[W, W + SESSION_MIN*60)`: all sessions open and in use; every session is planned to last until the end of this phase;
- cool-down: `COOLDOWN_MIN*60` seconds with no MCP traffic. mcpload computes `phases` from the same env vars, so `memory_leak`, `session_leak` and `fd_leak` work as in a soak.

Between rounds of `PARALLEL` calls the agent thinks (exponential, mean `THINK_MS`), then pauses a further `IDLE_MS`. When the session has been quiet for `PING_EVERY` seconds it sends a `ping`. A session **dies** when a call or ping gets `session_not_found`, or when `DEAD_ROUNDS` rounds in a row fail with transport errors only; the agent then reconnects and carries on. Calls in the first and last third of a session's planned lifetime are tagged `session_age=early|late`.

| Var | Default | Meaning |
|---|---|---|
| `VUS` | `20` | agents, one session each |
| `SESSION_MIN`, `WARMUP_MIN`, `COOLDOWN_MIN` | `10`, `1`, `2` | phase lengths in minutes |
| `THINK_MS` | `2000` | mean think time between rounds |
| `IDLE_MS` | `0` | extra fixed pause after every round (an agent that goes quiet) |
| `PING_EVERY` | `60` | ping after this many quiet seconds; `0` never pings |
| `DEAD_ROUNDS` | `3` | rounds of transport-only failures that count as a dead session |
| `SURVIVAL_MIN` | `1` | threshold on `mcp_session_survived` |

| Metric | Type | Meaning |
|---|---|---|
| `mcp_session_survived` | Rate | 1 for a session that lived to its planned end, 0 for one that died (tag `cause`) |
| `mcp_session_lifetime` | Trend (ms) | how long each session lasted (tags `outcome=survived\|died`, `cause`) |
| `mcp_session_reconnects` | Counter | reconnects after a death |

**Idle expiry.** Servers reap sessions that stay idle too long (the TS demo after `SESSION_IDLE_MS`, 5 min). With the defaults a session is never quiet that long. To test idle expiry, pause longer than the server's timeout without pinging, then compare with pinging:

```sh
docker run -d --rm --name mcpload-idle-ts -p 127.0.0.1:3020:3000 -e SESSION_IDLE_MS=20000 mcpload-chaos/ts-server:local   # image: see demo-servers/README.md
./mcpload run --scenario long-lived --url http://localhost:3020/mcp --vus 10 --warmup-min 0.25 --cooldown-min 0.25 \
  --env SESSION_MIN=2 --env IDLE_MS=45000 --env PING_EVERY=0    # session_survival FAIL: sessions die with session_not_found
#   ... --env PING_EVERY=10                                      # PASS: pings keep the sessions alive
```

## Reconnect storm

`reconnect-storm.js` runs `VUS` agents for `DURATION`. Each holds a session and calls tools with think time. When a session breaks (`session_not_found`, or every call of a round failing with a transport error) the agent drops it without a DELETE and reconnects at once. Failed connects back off: `RECONNECT_BACKOFF_MS` before the second attempt, doubling up to `RECONNECT_MAX_MS`, with `JITTER` (0–1) taking up to that fraction off each wait at random. With all agents losing their session at the same moment, this is the reconnect storm. Run it through mcpload with `--chaos-restart` (see [cmd/mcpload/README.md](../cmd/mcpload/README.md#chaos-restart-and-the-recovery-verdict)); only restart servers you own.

Every `tools/call` carries a call id in `params._meta`:

```json
{ "method": "tools/call", "params": { "name": "search", "arguments": { "query": "x" }, "_meta": { "io.mcpload/callId": "3f9c2a1b-7-42" } } }
```

The id is `<CALL_ID_PREFIX>-<vu>-<n>`; mcpload sets a fresh prefix per run. A server that records the ids it executes can tell what really happened to the calls that were in flight when it went down. To support it on your own server, read `params._meta["io.mcpload/callId"]` in your tools/call handler where the tool acts, count executions per id, and serve them as `GET <your url>?prefix=<p>` → `{"executions": {"<id>": <count>, ...}}` for mcpload's `--calls-url` (the TS demo does this with `TRACK_CALLS=1`). A counter such as `mcp_tool_duplicate_executions_total` (ids run more than once) is a cheap production alarm for the same thing.

| Var | Default | Meaning |
|---|---|---|
| `VUS`, `DURATION` | `20`, `2m` | agents and run length |
| `RECONNECT_BACKOFF_MS`, `RECONNECT_MAX_MS` | `100`, `5000` | backoff between failed reconnect attempts (the first attempt is immediate); `0` retries in a hot loop |
| `JITTER` | `0` | fraction (0–1) of each backoff taken off at random |
| `RETRY_ON_ERROR` | `0` | `1`: re-send each call that failed with a transport error or `session_not_found`, **with the same call id**, on this session or the next one, up to `RETRY_MAX` attempts in all (3). A call whose response was lost in the restart then runs twice: the client-retry duplicate. |
| `DOUBLE_SEND` | `0` | share of calls sent twice at once with the same id (a hedged request, or two workers taking one job). Against a server whose duplicate check isn't atomic (`DEDUPE=racy` on the TS demo) both run. |
| `CALL_ID_PREFIX` | random | call id prefix; mcpload sets it |
| `RECOVERY_BUDGET`, `RECOVERY_WINDOW` | `30`, `5` | read by mcpload: seconds the server may take to recover, and how long a healthy stretch must last |

| Metric | Type | Meaning |
|---|---|---|
| `mcp_session_breaks` | Counter | broken sessions (tag `cause`) |
| `mcp_reconnects`, `mcp_reconnect_duration` | Counter, Trend (ms) | reconnects after a break, and the time from the break to the new session |
| `mcp_calls_tagged` | Counter | call ids sent |
| `mcp_call_attempts` | Counter | one sample per attempt of a call id that failed or was sent more than once (tags `call_id`, `outcome` = `answered` or the error type) |

## Syntax check and unit tests

`package.json` sets `"type": "module"`, so Node can parse these files. It doesn't run them.

```sh
for f in scenarios/lib/*.js scenarios/*.js; do node --check "$f"; done
```

The pure helpers (schema placeholders, tool selection, budget coverage, fractional rates, workflow plans, workload flows and templates, step schedules, version-skew classification, break detection, reconnect backoff, call ids, the MCP_URL / MCP_COMMAND target options) have unit tests that need only Node, not k6:

```sh
node scenarios/lib/schema-args.test.mjs
node scenarios/lib/workflow.test.mjs
node scenarios/lib/workload.test.mjs
node scenarios/lib/skew.test.mjs
node scenarios/lib/resilience.test.mjs
node scenarios/lib/cancel.test.mjs
node scenarios/lib/content.test.mjs
node scenarios/lib/config.test.mjs
```
