# mcpload documentation

[Back to the README](../README.md)

## Start here

- [Install and try mcpload](guide/getting-started.md): every install option, and a tour of the demo servers that pass and fail on purpose.
- [Quick start from source](QUICKSTART.md): build from source and run the demo servers on Windows, Linux and macOS.

## Guides

- [Test your own server](guide/testing-your-server.md): point mcpload at your server, choose tools, set per-tool budgets, auth, soak tests for leaks, getting trustworthy results, and running from Docker.
- [Scenarios and verdicts](guide/scenarios-and-verdicts.md): what mcpload checks, which test to run when, the built-in scenarios, and how to read every check in the result.
- [Find your capacity](guide/capacity.md): `mcpload capacity` and the estimated number of agents your server can take.
- [Workload profiles](guide/workloads.md): weighted business flows with test data and per-flow budgets, in one YAML file.
- [Local (stdio) servers](guide/stdio.md): load-test a server that runs as a subprocess (`--command`): start-up cost, head-of-line blocking, per-process leaks and stdout pollution. v0.6.0, unreleased.
- [Use mcpload in CI](guide/ci.md): the GitHub Action, comparing every pull request with `main`, and the "soak-tested" badge.
- [Advanced](guide/advanced.md): custom JavaScript scenarios, the metrics, and the repository layout.

## Reference

- [CLI reference](../cmd/mcpload/README.md): every command and flag, and how each verdict is computed.
- [Scenario settings](../scenarios/README.md): every `--env` setting of every scenario.
- [Report format](../report/schema/README.md): the `report.json` schema.
- [GitHub Action inputs and outputs](../action/action.yml)
- [Demo servers](../demo-servers/README.md): what each healthy and broken target does.

## Background

- [How mcpload compares](COMPARISON.md) with MCP Inspector, eval tools, JMeter, xk6-mcp and mcp-bench.
- [Common questions](FAQ.md)
- [How it works (architecture)](ARCHITECTURE.md)
- [Calibration results and recommended budgets](CALIBRATION.md)
- [Roadmap](ROADMAP.md)
- [Security policy](../SECURITY.md) and [threat model](THREAT_MODEL.md)
