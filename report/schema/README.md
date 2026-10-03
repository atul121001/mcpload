# report.json schema (v1)

`report.v1.json` is the JSON Schema (draft 2020-12) for the `report.json` that the `mcpload` CLI writes. Anything that consumes reports (the HTML renderer, or a server receiving `mcpload upload`) should treat this schema as the contract.

Validate a file:

```sh
cd report && npm ci
node validate.mjs path/to/report.json
```

`validate.mjs` runs the schema first, then a few checks that JSON Schema can't express:
- every series array has the same length as `series.t`;
- `t` is strictly increasing;
- phases are ordered;
- `byErrorType` adds up to `summary.errors`;
- per-tool percentiles are monotonic (p50 ≤ p95 ≤ p99 ≤ max);
- in `workflow`, `completed` ≤ `runs`, step names are unique, and its percentiles are monotonic too.
- `capacity.steps` are sorted by strictly increasing `vus`, with `errors ≤ reqs`, `startS ≤ endS` and per-tool `p95 ≤ p99`.

`mcpload validate` runs the same schema rules and semantic checks in Go (`report.(*Report).Check`), so both validators accept and reject the same reports.

## Versioning

- `schemaVersion` is a string holding the **major** version only: `"1"`.
- **Additive changes keep the major version.** Adding an optional field, a new `byErrorType` key, a new optional series, or a new enum value that consumers can treat as unknown all count as additive. Producers and consumers MUST ignore properties they don't recognise, which is why the schema doesn't set `additionalProperties: false`.
- **Breaking changes bump the major version** and ship as `report.v2.json` alongside v1. Breaking changes include removing or renaming a field, changing a type or unit, making an optional field required, or changing what a field means. Consumers should keep accepting every major version that has shipped.
- Enum values for `verdicts[].id` and `verdicts[].status` count as part of the contract. Adding an id is additive. Consumers should render an unknown id generically and treat an unknown status as `warn`.

## Units and conventions

| Kind | Unit |
|---|---|
| Timestamps (`run.startedAt`, `run.endedAt`) | RFC 3339, UTC |
| Durations and phase boundaries (`*S`) | seconds; `series.t` and phases count from run start |
| Latencies (`p50`, `p95`, `p99`, `max`, `*Ms`) | milliseconds |
| Rates (`errorRate`) | fraction from 0 to 1 |
| Memory (`*Bytes`) | bytes |
| `verdicts[].slopePerMin` | the signal's unit per minute (bytes/min for RSS) |

`null` in a series means no sample for that bucket. For example, `client.p95Ms` is `null` during cool-down when no requests run.

## Fields

| Field | Meaning |
|---|---|
| `schemaVersion` | `"1"` |
| `tool` | `{name, version}` of the program that wrote the report (`mcpload`) |
| `run.id` | unique run id; an upload server can use it to make uploads idempotent |
| `run.startedAt`, `run.endedAt`, `run.durationS` | wall-clock span of the run |
| `run.scenario` | `soak`, `agent-session`, `agent-workflow`, `burst`, `lb-check`, `isolation`, `step-load`, `oauth-refresh`, or a custom name |
| `run.protocol` | the protocol that was actually negotiated, e.g. `2025-06-18` or `2026-07-28` (never `auto`) |
| `run.target` | `{url, label?}`; `label` is a short display name |
| `run.git` | optional `{sha, ref?}` of the system under test |
| `run.k6Version` | version of the k6 binary |
| `run.generator` | optional `{cores, cpuAvgPct, cpuMaxPct}` for the load generator. The percentages are the share of the machine's **total** CPU capacity (all cores) used by the k6 process, 0–100, or `null` when not measured. `cpuAvgPct` averages over the whole k6 lifetime (including an idle cool-down); `cpuMaxPct` is the busiest full sampling interval. |
| `run.load` | `{executor, vus?, maxVus?, arrivalRate?, arrivalTimeUnitS?}`, the load shape during the constant-load phase |
| `phases` | `{warmupEndS, loadEndS, cooldownEndS}`. Warm-up is `[0, warmupEndS)`, constant load is `[warmupEndS, loadEndS)`, and cool-down (zero load) is `[loadEndS, cooldownEndS]`. A scenario without phases sets `warmupEndS = 0` and `cooldownEndS = loadEndS`. |
| `summary` | `{reqs, errors, errorRate, byErrorType}` from `mcp_reqs` and `mcp_errors`; the keys of `byErrorType` are `error_type` tag values. Optional: `iterations` (k6 `iterations`), `droppedIterations` (k6 `dropped_iterations`, iterations an arrival-rate executor could not start on time) and `toolErrors` (count of `error_type` `tool_iserror`). |
| `tools[]` | per tool: `{name, reqs, errors, errorRate, p50, p95, p99, max}`, taken from `mcp_req_duration{method:tools/call}` grouped by `tool`. The latency percentiles cover **successful calls only** (a fast error or a timeout would otherwise distort them); `errors`/`errorRate` still count every failed call. |
| `thresholds[]` | `{metric, expr, passed, observed}`, one entry per k6 threshold expression. `metric` is the k6 metric key including its tag selector (e.g. `mcp_req_duration{tool:search}`). `observed` is the value that `expr` aggregates, or `null` if there were no samples. |
| `series` | `intervalS`, `t[]`, `client {p95Ms[], errorRate[], rps[]}`, and `server {sampler, rssBytes[], heapBytes[]?, openFds[]?, activeSessions[]?}`. The arrays run in parallel with `t`. When `sampler` is `none`, `rssBytes` is `[]`. Optional: `client.droppedIterations[]` (dropped iterations per bucket) and `tools` — a map from tool name to `{p95Ms[]}`, the p95 of successful calls of that tool per bucket. |
| `verdicts[]` | `{id, status, signal, slopePerMin?, r2?, baseline?, cooldownRecovered?, message}`. `signal` names the series or metric the verdict was computed from (e.g. `server.rssBytes`). |
| `payloadsIncluded` | `true` only when the run used `--include-payloads` |
| `workflow` | optional; only in runs that emit the `agent-workflow` scenario's metrics. `{runs, completed, completionRate, durationMs, steps[]}`. `runs`/`completed` count `mcp_workflow_complete` samples (workflows started / that ran every step). `durationMs` is `{count, p50, p95, p99, max}` of `mcp_workflow_duration`: complete workflows, from `connect()` to the end of the last step, think pauses included. `steps[]` is `{name, count, p50, p95, p99, max}` of `mcp_workflow_step_duration` per `step` tag, in plan order: the wall time of each step's parallel batch (its slowest call). All latencies are 0 when `count` is 0. |
| `capacity` | optional, step-load runs only: `{plannedVus?, minAgents?, maxSustainableVus, breakingVus, inconclusive, stoppedEarly, budgets, steps[]}`. `maxSustainableVus` is the last step before the first breach (the highest step when none broke; `null` when the first step broke); `breakingVus` is the first step with a breach (`null` when none); `inconclusive` means k6 was saturated at the breaking step; `stoppedEarly` means the scenario's `ABORT_ERR_RATE` guard ended the run. `budgets` is `{connectP95Ms, connectErrorRate, tools: {name: {p95Ms, p99Ms, errorRate}}}`. Each step is `{vus, startS, endS, reqs, errors, errorRate, rps, connectP95Ms, connectErrorRate, generatorCpuMaxPct?, generatorSaturated, passed, breached?, breaches[], tools[]}`, covering the requests tagged `step=<vus>` (ramps are left out); `startS`/`endS` are its first and last sample; `breached` lists step-level fields over budget (`connectP95Ms`, `connectErrorRate`, `toolCalls`) and `breaches` describes every breach in words, worst first. `tools[]` are `{name, reqs, errors, errorRate, p95, p99, breached?}` with `p95`/`p99` over successful calls (`null` without one) and `breached` from `p95`, `p99`, `errorRate`. |

### Verdict ids

| id | Signal | Method |
|---|---|---|
| `memory_leak` | `server.rssBytes` or `server.heapBytes` | Judges RSS and, when present, heap; fails if either fails and `signal` names the failing one. Trend: OLS over the leak window fitted to the **lower envelope** (centred rolling minimum over 3 buckets, which strips the GC sawtooth), `fail` when slope > 1 MiB/min **and** R² ≥ 0.7 **and** fitted growth over the window ≥ 5 MiB. Residue: `fail` when the cool-down mean stays above the post-warm-up baseline by more than `clamp(1 MiB/min × window minutes, 5 MiB, 10 MiB)` (runtimes keep some memory, so a small residue passes; the 10 MiB cap stops long soaks from excusing a large one). |
| `session_leak` | `server.activeSessions` | Trend as above with limit 0.5/min, growth floor 3. Residue: the lowest cool-down sample is compared with the **idle** level (lowest sample before load, i.e. during warm-up; 0 without a warm-up phase), not with the under-load baseline — more than 2 sessions retained is `fail`. |
| `fd_leak` | `server.openFds` | Trend with limit 1/min, growth floor 5. Residue vs the idle level (warm-up minimum, else the first sample): more than 5 fds retained is `fail`. |
| `latency_drift` | `tools.<name>.p95Ms` (else `client.p95Ms`) | Computed per tool when `series.tools` is present, so one tool regressing can't hide in the mixed p95. `warn` when, for any tool, the rise over the load window exceeds 50% of its baseline and at least 25 ms, with R² ≥ 0.5; the message names the drifting tool(s). `skipped` with less than 2 min of load. |
| `error_drift` | `client.errorRate` | `warn` when the error rate rises more than 1 percentage point over the load window with R² ≥ 0.5. `skipped` with less than 2 min of load. |
| `session_not_found` | `mcp_errors{error_type:session_not_found}` | any occurrence is `fail` (LB without sticky sessions) |
| `threshold` | `thresholds` | `fail` if any k6 threshold failed |
| `tool_isolation` | `tools.p95Ms (solo vs mixed)` | Only in runs of the `isolation` scenario, which runs the same sessions in two k6 scenarios: `solo` (tool mix without the slow tools) and `mixed` (full mix). Compares each tool's p95 of successful calls between them, for tools with at least 30 calls in both. `fail` when, for any tool, mixed p95 is at least 2× solo p95 **and** at least 50 ms higher; `warn` at 1.5× and 25 ms. The absolute floors stop tools of a few ms from failing on noise. `skipped` when a phase is missing, the mixed phase called no extra tool (check `SLOW_TOOLS`), or no tool had enough calls. |
| `capacity` | `capacity.steps` | Only in runs whose requests carry a `step` tag (the `step-load` scenario). Each step is judged against the scenario budgets: per tool with at least 10 calls in the step, p95/p99 of successful calls against `P95_MS`/`P99_MS` and the error rate against `ERR_RATE` (with `TOOL_BUDGETS` overrides); connect p95 against `CONNECT_P95_MS` and failed session starts against `ERR_RATE`; a step without any `tools/call` is a breach. Value ≥ budget is a breach, as in k6. The breaking point is the first step with a breach; when k6's busiest CPU interval in that step was > 90% of the machine the result is inconclusive. With a `MIN_AGENTS` target: `pass` when max sustainable ≥ target, `fail` when a conclusive breach happened at or below the target (or at the first step), else `warn`. Without a target: `pass` when no step broke, `fail` when the first step broke conclusively, else `warn`. `skipped` when no request carried a step tag. In these runs `latency_drift` and `error_drift` are `skipped`. |
| `generator` | `summary.droppedIterations` or `run.generator.cpuMaxPct` | Did the load generator deliver the requested load? dropped / (iterations + dropped) > 1% is `warn`, > 10% is `fail` (the results don't reflect the requested load); k6 CPU in its busiest sampling interval (`cpuMaxPct`) > 90% of the machine is `warn` (latency may include generator overhead; `cpuAvgPct` spans the whole run including the idle cool-down, so it is reported but not judged). `skipped` when neither is reported. |

**Leak window and short runs.** The leak verdicts regress over the constant-load window `[warmupEndS, loadEndS)`, but never start before 60 s into the run, so startup growth of a scenario without warm-up is not read as a leak. They are `skipped` ("needs a soak run with cool-down") when the run has no cool-down (`cooldownEndS <= loadEndS`) or the leak window is shorter than 2 minutes. With less than 10 minutes of load they are still judged, but the message says that only fast leaks (≳2 MiB/min) are reliably detected. A steep slope with R² below 0.7 is reported as "no consistent trend", never as flat.

`skipped` means the verdict could not be judged: the signal wasn't available (e.g. a server-side verdict when the sampler is `none`) or the run is too short for it.

### Overall result

A report **fails** if any `verdicts[].status` is `fail` or any `thresholds[].passed` is `false`. Otherwise it **passes**, with warnings if any verdict is `warn`. The HTML report and the CLI exit code both use this rule.
