# Roadmap

What mcpload can do today, and what's planned next. Design details are in [ARCHITECTURE.md](ARCHITECTURE.md).

## Done
- **k6 extension** (`k6/x/mcpload`): stateful (tested with 2025-06-18 and 2025-11-25) and stateless 2026-07-28 MCP protocols with `auto` negotiation; bearer and OAuth client-credentials auth; per-tool metrics.
- **Scenario library:** agent session, burst (initialize flood), soak, load-balancer check, OAuth refresh.
- **`mcpload` CLI:** runs k6, samples server memory (Docker or Prometheus), returns leak and drift verdicts, writes `report.json` and a self-contained `report.html`.
- **Report format:** versioned schema v1 with a validator.
- **Demo servers:** known-good and known-bad targets for calibration, bound to 127.0.0.1 ([results](CALIBRATION.md)).
- **GitHub Action:** gates PRs on per-tool budgets and leak verdicts.
- **Server-to-client requests:** sampling, elicitation, roots and ping requests inside SSE response streams are answered with static, optionally delayed responses (stateful protocols); metrics `mcp_server_requests` and `mcp_server_request_duration`.
- **Step-load scenario:** raises concurrency in steps and reports the breaking point (verdict `capacity`): the last step that held every per-tool budget, the breach at the next one, and a per-step table and p95-vs-agents chart in the report. Each step also lists errors by class.
- **Agent workflows:** multi-step plans where later calls use earlier results (`agent-workflow`).
- **Version skew:** replicas on different builds behind one load balancer; verdict `version_skew` separates fast typed failures from hangs.
- **Chaos restart and call integrity:** `--chaos-restart` restarts the server's container mid-run; verdict `recovery` reports time to recover, and `call_integrity` compares the call ids the client sent (`_meta`) with what the server executed, to find lost and duplicated calls.
- **Long-lived sessions:** `long-lived` scenario and `session_survival` verdict.
- **Cancellation:** `notifications/cancelled` on cancel and timeout; verdict `cancellation` checks from server metrics whether cancelled work stopped.

## Next
- `Mcp-Param-*` headers, MRTR (`InputRequiredResult`) and `subscriptions/listen` from the 2026-07-28 spec.
- Payloads in reports: the extension accepts `includePayloads` and the CLI has `--include-payloads`, but tool arguments and results are not stored yet.
- Publish the extension to the k6 extension registry, so stock k6 can fetch it automatically.
- Chaos inputs for the GitHub Action (`--chaos-restart` is CLI-only today).
- Answering MRTR (`InputRequiredResult`) so sampling and elicitation also work on stateless 2026-07-28 servers.
- Load tests for resources and prompts, and for stdio and SSE servers.

Ideas and bug reports are welcome. See [CONTRIBUTING.md](../CONTRIBUTING.md).
