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
- per-tool percentiles are monotonic (p50 ≤ p95 ≤ p99 ≤ max).

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
| `run.scenario` | `soak`, `agent-session`, `burst`, `lb-check`, `oauth-refresh`, or a custom name |
| `run.protocol` | the protocol that was actually negotiated, e.g. `2025-06-18` or `2026-07-28` (never `auto`) |
| `run.target` | `{url, label?}`; `label` is a short display name |
| `run.git` | optional `{sha, ref?}` of the system under test |
| `run.k6Version` | version of the k6 binary |
| `run.load` | `{executor, vus?, maxVus?, arrivalRate?, arrivalTimeUnitS?}`, the load shape during the constant-load phase |
| `phases` | `{warmupEndS, loadEndS, cooldownEndS}`. Warm-up is `[0, warmupEndS)`, constant load is `[warmupEndS, loadEndS)`, and cool-down (zero load) is `[loadEndS, cooldownEndS]`. A scenario without phases sets `warmupEndS = 0` and `cooldownEndS = loadEndS`. |
| `summary` | `{reqs, errors, errorRate, byErrorType}` from `mcp_reqs` and `mcp_errors`; the keys of `byErrorType` are `error_type` tag values |
| `tools[]` | per tool: `{name, reqs, errors, errorRate, p50, p95, p99, max}`, taken from `mcp_req_duration{method:tools/call}` grouped by `tool` |
| `thresholds[]` | `{metric, expr, passed, observed}`, one entry per k6 threshold expression. `metric` is the k6 metric key including its tag selector (e.g. `mcp_req_duration{tool:search}`). `observed` is the value that `expr` aggregates, or `null` if there were no samples. |
| `series` | `intervalS`, `t[]`, `client {p95Ms[], errorRate[], rps[]}`, and `server {sampler, rssBytes[], heapBytes[]?, openFds[]?, activeSessions[]?}`. The arrays run in parallel with `t`. When `sampler` is `none`, `rssBytes` is `[]`. |
| `verdicts[]` | `{id, status, signal, slopePerMin?, r2?, baseline?, cooldownRecovered?, message}`. `signal` names the series or metric the verdict was computed from (e.g. `server.rssBytes`). |
| `payloadsIncluded` | `true` only when the run used `--include-payloads` |

### Verdict ids

| id | Signal | Method |
|---|---|---|
| `memory_leak` | `server.rssBytes` | OLS slope over the constant-load window. `fail` when the slope is above the limit (MiB/min, 1 MiB = 1,048,576 bytes) **and** R² ≥ 0.7, or when cool-down does not return near the post-warm-up baseline. |
| `session_leak` | `server.activeSessions` | same method |
| `fd_leak` | `server.openFds` | same method |
| `latency_drift` | `client.p95Ms` | slope as a share of baseline over the window; usually `warn`, not `fail` |
| `error_drift` | `client.errorRate` | same as `latency_drift` |
| `session_not_found` | `mcp_errors{error_type:session_not_found}` | any occurrence is `fail` (LB without sticky sessions) |
| `threshold` | `thresholds` | `fail` if any k6 threshold failed |

`skipped` means the signal wasn't available, e.g. a server-side verdict when the sampler is `none`.

### Overall result

A report **fails** if any `verdicts[].status` is `fail` or any `thresholds[].passed` is `false`. Otherwise it **passes**, with warnings if any verdict is `warn`. The HTML report and the CLI exit code both use this rule.
