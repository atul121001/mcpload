# Scenarios and verdicts

[README](../../README.md) · [All docs](../README.md)

What mcpload checks, which test to run when, and how to read the result.

## What it checks

| The question you have | What mcpload does |
|---|---|
| **Can my server handle many agents at once?** | Simulates many agents opening sessions, listing tools and calling tools in parallel, then measures speed and errors for **each tool separately**. |
| **How many agents can it take before it breaks?** | `mcpload capacity --from 10 --to 1000` raises the number of agents step by step, tells you the last level where every tool stayed within budget and which tool broke first, and estimates the capacity in between (e.g. "~250 agents"). |
| **Do real multi-step tasks stay fast?** | Runs agents through a plan where each step's calls are built from the previous step's results, and times every step and the whole task. |
| **Does a realistic business workload hold up?** | Runs a mix of business flows from a YAML file (say 60% "look up orders", 30% "check subscription", 10% "open a ticket") with test data, and checks each flow's end-to-end time and completion rate against its own budget. |
| **Does one slow tool hold up the others?** | Compares each tool's speed alone and mixed with your slow tools, to catch a shared pool or a blocked event loop. |
| **Do sampling and elicitation work under load?** | Answers the server's mid-call requests (sampling, elicitation, roots) with a configurable delay, like a real client waiting on an LLM or a person. |
| **Does it slowly run out of memory?** | Runs a long test (30–60 minutes is typical), samples the server's memory as it goes, and tells you if memory keeps climbing and never comes back down. |
| **Does it lose sessions behind a load balancer?** | Spots the classic "session not found" failure when requests from one agent land on different servers. |
| **What happens during a rolling deploy?** | Puts agents on replicas running different builds and tells you whether a mismatch fails fast with a clear error or leaves clients hanging until they time out. |
| **What if the server restarts mid-run?** | Restarts your server's container during the test (`--chaos-restart`), measures how long until agents are served again, and counts tool calls that were lost or **ran twice**. |
| **Do long-lived agent sessions survive?** | Keeps one session per agent open for many minutes and reports sessions the server dropped while they were still in use. |
| **Does the server stop work when an agent cancels?** | Cancels a share of tool calls and checks, from the server's own metrics, whether the cancelled work actually stopped or kept using capacity. |
| **Is it ready for the newer stateless MCP spec?** | Speaks both the older session-based protocol and the stateless 2026-07-28 protocol, and picks the right one automatically. |
| **Do logins survive under load?** | Tests OAuth token refresh with many agents at once. |
| **Did my latest change make it slower?** | Runs in CI on every pull request, compares each tool with the last run on `main` ("`search` p95 210 ms → 284 ms, +35%"), and fails the build on a real regression or when a tool goes over its time or error budget. Noise floors keep a busy CI runner from failing the build. |

## Which test should I run?

| When | Run | How long |
|---|---|---|
| On every pull request | `agent-session` (the default) in CI, with per-tool budgets, compared with `main` (`baseline-branch: main`) | 1–2 min |
| Before a release | `mcpload capacity` to find how many agents you can take, then `soak` with a sampler to catch leaks | 10–60 min |
| You know how agents really use your tools | `--workload` with your flows and their weights, budgeted per flow | 2–10 min |
| You run more than one replica | `lb-check`, then `version-skew` with two builds behind the load balancer | 2–5 min |
| You changed timeouts, restarts or deploys | `reconnect-storm` with `--chaos-restart`, plus `--calls-url` to count lost and duplicated calls | 2–5 min |
| Agents keep sessions open for long | `long-lived` | 10+ min |
| Your tools are slow or cancellable | `isolation`, and `agent-session` with `CANCEL_RATE` | 2–5 min |
| Your server runs locally over stdio (v0.6.0, unreleased) | `--command` with `agent-session`, `isolation` and `long-lived`; see [stdio servers](stdio.md) | 2–20 min |

Every option is in [scenarios/README.md](../../scenarios/README.md).

## Built-in scenarios

| Scenario | What it simulates |
|---|---|
| `agent-session.js` | Agents opening a session, listing tools and calling several in parallel, with pauses in between. |
| `workload.js` | A workload profile: each agent session picks a business flow by weight and runs its steps with test data. Reports every flow's time and completion rate. Use it with `--workload <file.yaml>`. |
| `agent-workflow.js` | Agents working through a multi-step plan: parallel calls, a pause to decide, then calls built from the earlier results. Reports each step's time and the whole workflow's time. Set your own plan with `--env WORKFLOW=...`. |
| `burst.js` | A sudden rush of new agents, including a flood of session starts. |
| `soak.js` | Steady traffic for a long time, then a quiet period, to find leaks. |
| `lb-check.js` | Session handling behind a load balancer. |
| `isolation.js` | Whether fast tools wait behind slow ones (a shared connection pool, worker pool or blocked event loop). Name your slow tools with `--env SLOW_TOOLS=...`. |
| `version-skew.js` | A rolling deploy caught halfway: replicas on two builds behind one load balancer. Reports whether mismatched requests fail fast with a typed error or hang until the timeout. Add `--env TOOLS_CACHE_TTL=5m` to reuse an earlier tool list. |
| `step-load.js` | More and more agents at once, in steps (10, 25, 50, 100, 200 by default), to find the breaking point. Set the steps with `--env STEPS=...` and a target with `--min-agents`. |
| `long-lived.js` | Agents that each keep one session open for a long time (10 minutes by default), calling tools now and then and pinging when quiet, to catch servers that drop live sessions or slow down as a session ages. |
| `reconnect-storm.js` | Agents that reconnect as soon as their session breaks. With `--chaos-restart` mcpload restarts the server mid-run and measures how fast it recovers, and with `--calls-url` whether any call was lost or ran twice. Only on servers you own. |
| `oauth-refresh.js` | Many agents sharing short-lived login tokens. |

Settings for each: [scenarios/README.md](../../scenarios/README.md).

## What you get

Every run ends with a clear verdict in the terminal, an exit code your CI understands, and a report you can open in any browser (`report.html`) or keep as data (`report.json`).

The images below are from real 6-minute runs against two of the bundled demo servers: one healthy, and one with a deliberate memory leak.

**A server with a memory leak.** Under steady load, memory climbs in a straight line. When the load stops (the blue "cool-down" area on the right), it stays up. mcpload marks this as a fail.

![Memory chart of the leaking demo server: memory rises from about 120 MiB to 370 MiB under steady load and stays there after the load stops. Marked FAIL.](../images/leaky-memory.png)

**A healthy server.** Memory settles after warm-up and stays flat. Marked as a pass.

![Memory chart of the healthy demo server: memory stays around 92 MiB under steady load. Marked PASS.](../images/healthy-memory.png)

The terminal tells the same story in words:

```text
  FAIL     memory_leak        RSS grew 64.14 MiB/min (R²=1.00) under constant load, above the 1 MiB/min limit,
                              and did not recover in cool-down (187.85 MiB above the post-warm-up baseline).
  FAIL     session_leak       Active sessions grew 60.00/min (R²=1.00) under constant load, above the 0.5/min
                              limit, and did not recover in cool-down.
  PASS     fd_leak            Open file descriptors flat under constant load.
  PASS     latency_drift      Client p95 stable.
  PASS     error_drift        Error rate did not trend upward over the load window.
  PASS     session_not_found  No 404 session-not-found responses.
  PASS     threshold          All 17 thresholds passed.
mcpload result: FAIL
```

(Long lines are wrapped and some messages shortened here.)

In plain words: memory and open sessions kept growing while the load stayed the same, and they didn't drop after the load stopped. Note that every speed budget passed. A leak like this doesn't show up in a short test; it shows up in production, hours later.

<details>
<summary><b>See more of the report</b></summary>

The top of the report: overall result, run details, and a card for every check.

![Top of the HTML report for the leaking server: FAIL, with memory leak and session leak checks failed and the other checks passed.](../images/leaky-summary.png)

Speed and errors for each tool, from the healthy run. Any cell over its budget is highlighted.

![Per-tool table: requests, errors, error rate, p50, p95, p99 and max latency for the tools search, fast, big, flaky and slow.](../images/healthy-tools.png)

</details>

## Reading the result

| You see | It means |
|---|---|
| **PASS**, exit code `0` | Every check and every budget passed. |
| **FAIL**, exit code `1` | At least one check or budget failed. The report says which, and why. |
| exit code `2` | The test itself couldn't run, for example a wrong URL or a scenario file that doesn't exist. |

The checks, in plain words:

| Check | Fails when… |
|---|---|
| `memory_leak` | Memory keeps rising under steady load and doesn't come back down afterwards. Soak tests only. |
| `session_leak` | Open sessions pile up and aren't cleaned up. Soak tests only. |
| `fd_leak` | Open files or connections pile up. Soak tests only. |
| `latency_drift` | A tool gets noticeably slower the longer the test runs (a warning). Each tool is checked separately. |
| `error_drift` | Errors become more frequent over time (a warning). |
| `session_not_found` | The server "forgets" an agent's session, which often means a load-balancer problem. |
| `threshold` | A tool went over your time or error budget. |
| `generator` | The test machine couldn't keep up: it dropped more than 1% of the planned load (a warning) or more than 10% (a fail), or mcpload's load generator itself went over 90% CPU at its busiest, so the speed numbers may include the test machine's own delay. See [Getting trustworthy results](testing-your-server.md#getting-trustworthy-results). |
| `version_skew` | Version-skew runs only: requests that reached a replica on a different build. A warning when they all fail fast with a typed error clients can handle (e.g. `Unsupported protocol version`), a fail when any of them hangs until the client timeout. |
| `capacity` | `mcpload capacity` and step-load runs only: how many agents at once the server held within your time and error budgets, and what broke at the next step, e.g. "Held budgets up to 50 agents; at 100 agents `slow` p95 1.9 s > 800 ms". Fails if your `--target` (`--min-agents` with `run`) isn't met (or, without a target, if even the first step breaks). Says "inconclusive" instead of blaming the server when the test machine was maxed out. |
| `session_survival` | Long-lived runs only: fails when sessions die before their planned end, e.g. "18 of 50 sessions died after a median 5m02s with session_not_found"; warns when calls late in a session are much slower than early ones. |
| `recovery` | Runs with `--chaos-restart`: how long the server took to serve normally again after mcpload restarted its container, e.g. "20 agents reconnected within 2.0 s; errors returned to under 1% 2.0 s after the restart (budget 30 s)". Fails over `RECOVERY_BUDGET`. |
| `call_integrity` | Runs whose calls carry call ids, with `--calls-url`: fails when the server ran a call twice (a client retry or a racy duplicate check), warns when calls failed on the client but ran on the server. |
| `regression` | Runs with `--baseline` (in CI: `baseline-branch: main`): fails when a tool got slower or more error-prone than in the baseline report beyond the noise floors, e.g. "`search` p95 210 ms → 284 ms (+35%, +74 ms)". Warns when the two runs used a different scenario, protocol or load, because the comparison may be unfair. See [Compare with main](ci.md#compare-with-main). |
| `cancellation` | Runs that cancelled calls (`CANCEL_RATE`, or timeouts) only: whether the server stops work it was told to cancel, e.g. "The server kept running `slow` for a median 2.1 s after 480 cancels: cancelled work still uses capacity". Needs `--sampler prometheus` and a server that exposes `mcp_work_after_cancel_seconds` (the TS demo server does) to judge the server side: a warning from a median of 100 ms of work after a cancel, a fail from 1 s. Without it, it reports what the client saw (cancels sent, late responses) and warns when more than 10% of cancelled calls still got a response. |

"Soak tests only" means the check is skipped on short runs without a cool-down, or with less than 2 minutes of steady load.
