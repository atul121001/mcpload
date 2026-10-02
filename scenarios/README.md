# Scenario library

These are k6 scripts for load- and soak-testing MCP servers. They need a k6 binary built with the `xk6-mcpload` extension. Stock k6 doesn't have `k6/x/mcpload`.

```sh
cd xk6-mcpload && xk6 build --with github.com/atul121001/mcpload/xk6-mcpload=. --output ../k6 && cd ..
```

On Windows, use `k6.exe` below. For a soak run, use the `mcpload` CLI rather than calling `k6 run` directly. The CLI wraps these scripts and adds server sampling, verdicts and `report.json`/`report.html`.

| Script | What it does | Default load |
|---|---|---|
| `agent-session.js` | Each VU loops through agent sessions: connect, `tools/list`, 1–5 rounds of `callParallel` with think time, then close. | 10 VUs for 2m |
| `burst.js` | Two phases. `init_flood` runs bare connect/close at 100/s to test the initialize storm. `agents` then ramps VUs from 0 to 200 in 10s. | about 1.5 min |
| `soak.js` | Agent sessions at a constant arrival rate. A warm-up ramp comes first and a zero-load cool-down comes last. | 3m warm-up + 30m load + 5m cool-down |
| `lb-check.js` | A stateful flow with sequential calls. Fails on any `session_not_found` or `header_mismatch`. | 10 VUs for 1m |
| `oauth-refresh.js` | 50 VUs on short-lived client-credentials tokens. Measures `mcp_oauth_refresh_duration` and counts auth errors. | 3m |

## Demo targets (`demo-servers/`, `docker compose up -d --build`)

| Port | Target | Try |
|---|---|---|
| 3001 | ts-healthy | `./k6 run -e MCP_URL=http://localhost:3001/mcp scenarios/agent-session.js` |
| 3002 | ts-leaky | `./k6 run -e MCP_URL=http://localhost:3002/mcp -e SOAK_MIN=30 scenarios/soak.js` |
| 3003 | py-healthy (stateless) | `./k6 run -e MCP_URL=http://localhost:3003/mcp scenarios/burst.js` |
| 3004 | lb-stateful (2 replicas, no sticky sessions) | `./k6 run -e MCP_URL=http://localhost:3004/mcp scenarios/lb-check.js` (expected to fail) |
| 3005 | stateless-2026 (2 replicas) | `./k6 run -e MCP_URL=http://localhost:3005/mcp -e MCP_PROTOCOL=2026-07-28 scenarios/lb-check.js` (expected to pass) |
| 3006 / 3007 | mock-oauth / ts-oauth | `./k6 run -e MCP_URL=http://localhost:3007/mcp -e OAUTH_TOKEN_URL=http://localhost:3006/token -e OAUTH_CLIENT_ID=mcpload -e OAUTH_CLIENT_SECRET=secret scenarios/oauth-refresh.js` |

Every demo server exposes the tools `fast`, `slow`, `flaky`, `big` and `search`.

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
| `TOOL_MIX` | `{"search":5,"fast":3,"slow":1,"big":1,"flaky":1}` | weights for picking tools. Tools the server doesn't list are skipped. |
| `TOOL_ARGS` | `{"search":{"query":"invoices","limit":5}}` | per-tool arguments. Any other tool gets placeholder values for its required properties, taken from its `inputSchema`. |
| `THINK_MS` | `500` | mean think time between rounds (exponential, capped at 5×) |
| `PARALLEL` | `3` | concurrent `tools/call` per round (`callParallel`) |
| `ROUNDS` | `1-5` | rounds per session, as `N` or `MIN-MAX` |
| `P95_MS`, `P99_MS`, `ERR_RATE` | `800`, `2000`, `0.01` | default per-tool budgets |
| `TOOL_BUDGETS` | `{"flaky":{"errRate":0.2}}` | per-tool overrides, e.g. `{"slow":{"p95":1500,"p99":2500}}` |
| `CONNECT_P95_MS` | `1500` | `mcp_connect_duration` p95 budget |
| `CHECKS_MIN` | `0.99` | minimum pass rate over all checks (`connect ok`, `tools/list returned tools`, `tools/call no transport error`, …) |
| `CONNECT_BACKOFF_MS` | `1000` | sleep after a failed `connect()` so a dead or unauthorised target is not hot-looped |

Each script has its own knobs, documented in its header comment: `VUS`, `DURATION`, `BURST_VUS`, `FLOOD_RATE`, `SOAK_MIN`, `WARMUP_MIN`, `COOLDOWN_MIN`, `RATE`, `STEPS`, `REFRESH_P95_MS`, and others.

## Thresholds

`lib/config.js#buildThresholds()` creates the following for every tool in `TOOL_MIX`:

```js
'mcp_req_duration{tool:<name>}':    ['p(95)<P95', 'p(99)<P99'],
'mcp_tool_error_rate{tool:<name>}': ['rate<ERR_RATE'],
'mcp_connect_duration':             ['p(95)<CONNECT_P95_MS'],
'checks':                           ['rate>=CHECKS_MIN'],
```

The `checks` threshold is what fails a run where no session ever starts (every `connect()` gets a 401 or is refused): the per-tool thresholds have no samples then, and k6 passes a threshold that has no samples.

Each scenario adds its own thresholds on top: `lb-check` adds `mcp_errors{error_type:session_not_found}: count<1`, `mcp_errors{error_type:header_mismatch}: count<1` and `checks{check:lb request ok}: rate>=LB_MIN_OK` (every request succeeds whichever replica serves it; this is the meaningful check for stateless servers, which have no session to lose), and `oauth-refresh` adds `mcp_oauth_refresh_duration` p95. If a threshold fails, k6 exits 99, which gates CI.

## Soak phases

`soak.js` has three phases:
- warm-up: `[0, W)`, with `W = WARMUP_MIN*60` (default is 10% of `SOAK_MIN`, at least 1 minute);
- load: `[W, W + SOAK_MIN*60)`;
- cool-down: the `COOLDOWN_MIN*60` seconds after load (default 5 minutes).

During cool-down a single idle VU sleeps, so there is no MCP traffic. Sessions still in flight may finish up to 30 s into cool-down. The CLI computes `phases` in `report.json` from the same env vars.

## Syntax check

`package.json` sets `"type": "module"`, so Node can parse these files. It doesn't run them.

```sh
for f in scenarios/lib/*.js scenarios/*.js; do node --check "$f"; done
```
