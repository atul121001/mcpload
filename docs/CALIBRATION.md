# Scenario calibration

Calibration rule (ARCHITECTURE §7): every verdict must fire on the bad target and stay quiet on the good one.
These runs check that each scenario in `scenarios/` gives the expected k6 exit code against the demo servers
in `demo-servers/`. Exit 0 means every threshold held. Exit 99 means at least one threshold was crossed.

Environment: Windows 11, `k6.exe v2.3.0` with `xk6-mcpload`, demo compose project `mcpload-demo`, run on
2026-09-30 and 2026-10-02. Each run was kept to 90 s or less and 50 VUs or fewer, because the host was shared
with other test runs. Durations are therefore shorter than the scenario defaults.

All numbers come from a single developer workstation and are environment-specific. Use them as a sanity check
for the demo servers, not as benchmarks; expect different absolute values on other hardware.

## Results

| # | Target | Scenario | Expected | Actual | Exit |
|---|---|---|---|---|---|
| 1 | 3001 ts-healthy | agent-session (10 VUs, 60 s) | pass | pass | 0 |
| 1 | 3003 py-healthy | agent-session (10 VUs, 60 s / 30 s) | pass | pass (run alone) | 0 |
| 1 | 3005 stateless-2026 | agent-session (10 VUs, 60 s) | pass | pass | 0 |
| 2 | 3001 ts-healthy | burst (`BURST_VUS=50 FLOOD_RATE=50 FLOOD_VUS=50 HOLD=20s`) | pass at this size | pass, with visible degradation | 0 |
| 2 | 3005 stateless-2026 | burst (same) | pass | pass | 0 |
| 3 | 3004 lb-stateful | lb-check (10 VUs, 1 m) | **fail**: session_not_found | **fail**: 6175 session_not_found, `lb request ok` 48.8 % | 99 |
| 3 | 3005 stateless-2026 | lb-check `MCP_PROTOCOL=2026-07-28` | pass | pass, 0 LB errors | 0 |
| 3 | 3005 stateless-2026 | lb-check `MCP_PROTOCOL=auto` | pass | pass (resolves to 2026-07-28) | 0 |
| 3 | 3005 stateless-2026 | lb-check `MCP_PROTOCOL=2025-11-25` | pass | pass (sessionless legacy) | 0 |
| 4 | 3007 ts-oauth + 3006 mock-oauth | oauth-refresh (20 VUs, 15 s + 60 s + 10 s) | pass, refresh measured | pass, 4 token fetches, 0 auth errors | 0 |
| 4 | 3007 ts-oauth + 3006 | oauth-refresh with `OAUTH_CLIENT_SECRET=wrong` | **fail**: auth | **fail**: `mcp_errors{error_type:auth}` and `checks` | 99 |
| 4b | 3007 ts-oauth | agent-session with no auth at all | **fail** | was exit 0 (bug), now **fail** on `checks` | 99 |
| 5 | 3001 ts-healthy | soak smoke (`SOAK_MIN=0.75 WARMUP_MIN=0.25 COOLDOWN_MIN=0.25 RATE=2`) | runs end to end | 3 phases ran, all thresholds held | 0 |
| 6 | 3003 py-healthy | agent-session with `TOOL_BUDGETS={"slow":{"p95":100},...}` | **fail** on slow p95 | **fail**: `mcp_req_duration{tool:slow}` p95 = 484 ms | 99 |
| 7 | 3008 ts-pooled | isolation (20 VUs, 1 m per phase), via `mcpload run` (2026-10-03) | **fail**: tool_isolation | **fail**: all 4 tools wait behind `slow`, e.g. `fast` p95 38 ms solo → 936 ms mixed (×24.6) | 1 |
| 7 | 3001 ts-healthy | isolation (same) | pass | pass: largest change `big` p95 48.5 → 47.4 ms (×0.98) | 0 |
| 7 | 3001 ts-healthy | isolation with `SLOW_TOOLS=nope` (10 s per phase) | **fail**: misconfiguration | **fail**: `checks{check:slow tools in mix}` = 0; `tool_isolation` skipped | 1 |

## Key metrics per run

The per-tool p95 values come from `mcp_req_duration{tool:*}`. The error breakdown comes from `--out json`,
aggregated by `error_type`, method and scenario.

**1. agent-session, good targets (60 s, 10 VUs, default budgets)**

| Target | fast | search | big | flaky | slow | connect p95 | checks | mcp_errors |
|---|---|---|---|---|---|---|---|---|
| 3001 | 10.8 ms | 11.8 ms | 18.8 ms | 13.5 ms | 311 ms | 19.7 ms | 100 % (1911) | 38, all `tool_iserror` (flaky 10.0 %) |
| 3003 | 18.4 ms | 19.3 ms | 25.0 ms | 20.5 ms | 316 ms (p99 1.26 s) | 8.4 ms | 100 % (1940) | 42, all `tool_iserror` (flaky 11.4 %) |
| 3005 | 6.3 ms | 6.8 ms | 18.0 ms | 6.6 ms | 307 ms | 7.3 ms | 100 % (1914) | 43, all `tool_iserror` (flaky 10.7 %) |

Protocol resolution with `auto`: 3001 and 3004 fall back to `2025-11-25` after a `server/discover` 400. 3003
and 3005 use `2026-07-28`.

Contention caveat: one 30 s run against 3003 failed (`mcp_connect_duration` p95 1.66 s, `fast` and `big` p95
over 800 ms) while four k6 processes were running on the same host. Run alone, it passed: connect p95 50 ms,
slow p95 347 ms. py-healthy is the most sensitive target to host CPU contention, so verdicts against it should
only be trusted from runs that had the host to themselves.

**2. burst (50 flood connects/s for 25 s, then 0→50 agent VUs in 10 s, held 20 s)**

| Target | flood connect p95 | agents connect p95 | fast p95 | search p95 | slow p95 | big p95 | mcp_errors |
|---|---|---|---|---|---|---|---|
| 3001 | 80 ms | 330 ms | 304 ms | 287 ms | **715 ms** | 437 ms | 54 `tool_iserror` only |
| 3005 | 38.5 ms | 84 ms | 132 ms | 133 ms | 436 ms | 317 ms | 93 `tool_iserror` only |

The initialize flood did no damage at 50/s: no errors, and flood connect p95 stayed far under the 2 s budget.
The damage shows up in the agent ramp. At 50 concurrent agents on the single Node process (3001), every tool
gains about 300–400 ms of event-loop queueing. `slow` reaches 715 ms against an 800 ms budget, and stateful
connect p95 grows 17× (19.7 ms to 330 ms), because every session costs 3 extra round trips: `initialize`,
`notifications/initialized` and `DELETE`. The stateless Go pair (3005) degrades far less. At the scenario
defaults (`BURST_VUS=200`, `FLOOD_RATE=100`), which were not run here because of the 50-VU limit, 3001 is
expected to cross the `slow` p95 budget.

**3. lb-check**

- 3004, exit 99. `mcp_errors` by type: `session_not_found` on `notifications/initialized` 1968, `tools/call`
  2309, `tools/list` 693, `DELETE` 628, `ping` 577. The run also had 36 `tool_iserror` and 1 `http` (a 502 from
  nginx). Of 3285 sessions, 60 % lost their session during the connect handshake. Crossed thresholds:
  `mcp_errors{error_type:session_not_found}`, `checks{check:lb request ok}`, and every
  `mcp_tool_error_rate{tool:*}` (about 46–51 %).
- 3005, exit 0. 2026-07-28: 879 sessions, 2637 `server/discover`, 7032 `tools/call`, all 200. The only errors
  were 74 `tool_iserror`. `lb request ok` was 100 %. 3005 round-robins per request (`X-Served-By` alternates
  between a and b), so the 0 errors cover both replicas.

**4. oauth-refresh (3007 + mock-oauth 3006, 30 s tokens, 20 VUs, 85 s total)**

- Exit 0, 12084 `tools/call`, 2014 sessions, 0 `auth` errors. The ts-oauth counter `mcp_auth_rejected_total`
  did not change during the run.
- `mcp_oauth_refresh_duration` recorded exactly **4 samples** for 20 VUs, at t+0 s, +24 s, +48 s and +72 s,
  all HTTP 200. The first took 30.7 ms (cold), the others 3–4 ms, so p95 was 26.6 ms. A 24 s interval is a
  refresh at 80 % of the 30 s lifetime. One fetch per refresh across all VUs confirms that the token cache is
  shared per credential set behind a single in-flight fetch: there was no storm. `oauth_tokens_issued_total`
  rose by 4.
- Negative test (`OAUTH_CLIENT_SECRET=wrong`, 5 VUs, 35 s), exit 99. 109 `auth` errors on `server/discover`
  (status tag `0`: the request never left the client), and `checks` was 0 %.

**5. soak smoke (3001)**: the warm-up 0–15 s, load 15–60 s and cool-down 60–75 s phases all ran. 107
sessions, 942 `tools/call`, checks 100 %, and `mcp_errors` was 11 `tool_iserror` (4 in warmup, 7 in load).
p95 values: fast 84 ms, search 88 ms, slow 440 ms. 3001 was shared with other runs at the time.

**6. tight budget**: `slow` p95 was 484 ms against a 100 ms budget. That threshold failed with exit 99 and
every other threshold held, which shows per-tool thresholds work in isolation.

## Commands

On Windows, use `./k6.exe`. `OAUTH_CLIENT_ID=mcpload` / `OAUTH_CLIENT_SECRET=secret` are the demo mock-oauth
credentials, not real ones. For the error breakdown, add `--out json=points.json` and aggregate the
`mcp_errors` points by `data.tags.error_type`.

```sh
# 1
./k6.exe run -e MCP_URL=http://localhost:3001/mcp -e DURATION=60s scenarios/agent-session.js
./k6.exe run -e MCP_URL=http://localhost:3003/mcp -e DURATION=60s scenarios/agent-session.js
./k6.exe run -e MCP_URL=http://localhost:3005/mcp -e DURATION=60s scenarios/agent-session.js
# 2
./k6.exe run -e MCP_URL=http://localhost:3001/mcp -e BURST_VUS=50 -e FLOOD_RATE=50 -e FLOOD_VUS=50 -e HOLD=20s scenarios/burst.js
./k6.exe run -e MCP_URL=http://localhost:3005/mcp -e BURST_VUS=50 -e FLOOD_RATE=50 -e FLOOD_VUS=50 -e HOLD=20s scenarios/burst.js
# 3
./k6.exe run -e MCP_URL=http://localhost:3004/mcp scenarios/lb-check.js                              # exit 99
./k6.exe run -e MCP_URL=http://localhost:3005/mcp -e MCP_PROTOCOL=2026-07-28 scenarios/lb-check.js  # exit 0
./k6.exe run -e MCP_URL=http://localhost:3005/mcp -e MCP_PROTOCOL=2025-11-25 scenarios/lb-check.js  # exit 0
# 4
./k6.exe run -e MCP_URL=http://localhost:3007/mcp -e OAUTH_TOKEN_URL=http://localhost:3006/token \
  -e OAUTH_CLIENT_ID=mcpload -e OAUTH_CLIENT_SECRET=secret -e VUS=20 -e DURATION=60s scenarios/oauth-refresh.js
./k6.exe run -e MCP_URL=http://localhost:3007/mcp -e OAUTH_TOKEN_URL=http://localhost:3006/token \
  -e OAUTH_CLIENT_ID=mcpload -e OAUTH_CLIENT_SECRET=wrong -e VUS=5 -e DURATION=10s scenarios/oauth-refresh.js   # exit 99
./k6.exe run -e MCP_URL=http://localhost:3007/mcp -e VUS=5 -e DURATION=10s scenarios/agent-session.js            # exit 99
# 5
./k6.exe run -e MCP_URL=http://localhost:3001/mcp -e SOAK_MIN=0.75 -e WARMUP_MIN=0.25 -e COOLDOWN_MIN=0.25 -e RATE=2 scenarios/soak.js
# 6
./k6.exe run -e MCP_URL=http://localhost:3003/mcp -e DURATION=30s -e VUS=5 \
  -e TOOL_BUDGETS='{"flaky":{"errRate":0.3},"slow":{"p95":100}}' scenarios/agent-session.js                      # exit 99
# 7 (mcpload exit 1 = FAIL, 0 = PASS)
./mcpload run --url http://localhost:3008/mcp --scenario isolation --duration 1m                                # FAIL tool_isolation
./mcpload run --url http://localhost:3001/mcp --scenario isolation --duration 1m                                # PASS
./mcpload run --url http://localhost:3001/mcp --scenario isolation --duration 10s --env SLOW_TOOLS=nope          # FAIL checks
```

## Recommended default budgets for the demo servers

| Budget | Value | Why |
|---|---|---|
| `P95_MS` / `P99_MS` | 800 / 2000 (unchanged) | Healthy p95 is ≤ 25 ms (≤ 350 ms for `slow`), so this leaves room for host noise. |
| `slow` | `{"p95":800,"p99":2000}` for ≤ 10 VUs; `{"p95":1500,"p99":2500}` for burst/200 VUs | The 300 ms floor plus queueing reached 715 ms at 50 VUs on 3001. |
| `flaky` errRate | **0.2** (was 0.15) | The tool fails 10 % by design. With about 100 calls, 15 % is only about 1.7 standard deviations away, so the old budget false-failed about 5 % of short runs. Observed rates were 4.7–12.8 %. |
| `ERR_RATE` | 0.01 | Healthy targets showed 0 non-flaky tool errors. |
| `CONNECT_P95_MS` | 1500 | Healthy p95 ≤ 60 ms, 330 ms under a 50-VU burst. |
| `CHECKS_MIN` | 0.99 (new) | Healthy runs showed 100 %. 3004 showed 48–75 %. |
| `LB_MIN_OK` | 0.99 (new, lb-check) | 3005 showed 100 %. 3004 showed 48.8 %. |
| `REFRESH_P95_MS` | 500 | Observed 27 ms. |
| `FLOOD_CONNECT_P95_MS` | 2000 | Observed 38–80 ms at 50/s. |

## Scenario fixes made during calibration

- `lib/config.js` `buildThresholds()`: added `checks: rate>=CHECKS_MIN` (0.99). Before this, a run where every
  `connect()` failed **exited 0**: agent-session against 3007 with no token had 0/5437 checks passing and still
  exited 0. The per-tool thresholds had no samples, and k6 passes thresholds that have no samples.
- `lib/session.js` `agentSession()`: a failed `connect()` now sleeps `CONNECT_BACKOFF_MS` (1000) before
  returning, and a successful one records `connect ok: true`, so the check is a real rate. Before this, the
  wrong-secret run hot-looped at 1745 iterations/s and sent 18 185 token requests in 35 s. Now it makes 109
  attempts.
- `lib/session.js`: `tool_iserror` results (the expected `flaky` failures) are no longer logged as warnings. They
  are still counted in `mcp_tool_error_rate`.
- `lib/config.js`: the default `flaky` budget changed from 0.15 to 0.2 (see above).
- `lb-check.js`: a check set is recorded for every request (connect, `tools/list`, each `tools/call`, `ping`).
  Before, it was recorded only for `tools/call`, so the 60 % of sessions that failed during the handshake left
  no check. The new check `lb request ok` (no `http`, `jsonrpc`, `timeout`, `session_not_found` or
  `header_mismatch` error) has the threshold `LB_MIN_OK`. This makes the scenario meaningful for stateless
  servers, which have no session to lose and can only fail by replicas disagreeing. A `ping` failure no longer
  aborts the session, a failed connect sleeps 50 ms instead of hot-looping, and VU 1 logs whether the target
  turned out stateful or stateless.

## Known limits

- **Leak detection floor.** With a 3-minute load window, the leak verdicts reliably catch growth of about
  2 MiB/min or more. Slower leaks get lost in normal memory noise (GC, caches, allocator behaviour). Use at
  least 10 minutes of steady load when hunting leaks; 30–60 minutes for slow ones. Leak verdicts are skipped
  entirely on runs without a cool-down or with less than 2 minutes of load.
- **Generator saturation.** When k6 runs short of CPU or can't start iterations on schedule, the latency it
  records includes its own delay, and the server looks slower than it is. The `generator` verdict flags this
  (dropped iterations above 1 % or k6 CPU above 90 % at its peak). The contention caveat above is an example of what
  happens without it. Treat any run with a `generator` warning or failure as unreliable for latency.
- **One workstation.** Every number in this document comes from a single Windows developer machine, with the
  demo servers and k6 on the same host. Absolute latencies and thresholds will differ elsewhere; recalibrate
  on your own hardware before relying on tight budgets.

## Known limitations and follow-ups

- **Extension, OAuth**: a failed token fetch is not cached or backed off. Before the scenario-side backoff,
  61 084 connects produced 18 185 token requests, so single-flight only collapses requests that are truly
  concurrent. A short negative cache (for example 1 s after a 4xx from the token endpoint) would protect real
  IdPs. Requests that fail before they are sent are tagged `status=0` in `mcp_reqs`. That is correct, but
  it is worth labelling clearly wherever the metric is displayed.
- **Extension, `rememberProtocol`**: the resolved protocol is remembered per Client, which means per VU. Every
  new VU repeats the `server/discover` probe: 100 probes in burst on 3001 and 35 in a 75 s soak. Under an
  arrival-rate flood this adds one round trip per new VU to `mcp_connect_duration`. A process-wide cache keyed
  by URL would remove that. In lb-check on 3004, VUs whose first connect failed re-probed, which suggests the
  protocol is only remembered after a full successful connect.
- **CLI / report**: exit 99 together with a low `checks` rate is better reported as "target unreachable or
  unauthorised" than as a latency regression. Showing `mcp_errors` split by `error_type` and method would also
  help: `session_not_found` on `notifications/initialized` is the main symptom of a missing sticky-session
  LB.
- **Demo servers**: after the lb-check runs, `lb-stateful-b` sat at 255 MiB of its 256 MiB limit, while
  `lb-stateful-a` used 40 MiB. Sessions whose `DELETE` went to the other replica stay alive until the 5 minute
  idle timeout. Repeated lb-check runs within 5 minutes may OOM-restart that replica, which would add
  `http` 502 errors on 3004. The verdict there is still a fail.
