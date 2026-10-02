# Roadmap

What mcpload can do today, and what's planned next. Design details are in [ARCHITECTURE.md](ARCHITECTURE.md).

## Done
- **k6 extension** (`k6/x/mcpload`): stateful (tested with 2025-06-18 and 2025-11-25) and stateless 2026-07-28 MCP protocols with `auto` negotiation; bearer and OAuth client-credentials auth; per-tool metrics.
- **Scenario library:** agent session, burst (initialize flood), soak, load-balancer check, OAuth refresh.
- **`mcpload` CLI:** runs k6, samples server memory (Docker or Prometheus), returns leak and drift verdicts, writes `report.json` and a self-contained `report.html`.
- **Report format:** versioned schema v1 with a validator.
- **Demo servers:** known-good and known-bad targets for calibration, bound to 127.0.0.1 ([results](CALIBRATION.md)).
- **GitHub Action:** gates PRs on per-tool budgets and leak verdicts.

## Next
- Answer server-to-client requests inside SSE streams (sampling, elicitation).
- `Mcp-Param-*` headers, MRTR (`InputRequiredResult`) and `subscriptions/listen` from the 2026-07-28 spec.
- A step-load scenario that finds the breaking point automatically.
- Payloads in reports: the extension accepts `includePayloads` and the CLI has `--include-payloads`, but tool arguments and results are not stored yet.
- Publish the extension to the k6 extension registry, so stock k6 can fetch it automatically.
- Tagged releases, so the GitHub Action can be pinned to a version.

Ideas and bug reports are welcome. See [CONTRIBUTING.md](../CONTRIBUTING.md).
