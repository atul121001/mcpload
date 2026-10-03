# Find your capacity

[README](../../README.md) · [All docs](../README.md)

Instead of running `--vus 10`, then 50, then 100 by hand, let mcpload step through them in one run:

```bash
mcpload capacity --url https://staging.example.com/mcp --env MCP_TOKEN=your-token \
  --from 10 --to 1000 --step-duration 1m --refine 2 --html capacity.html
```

It runs 10, 20, 40, ... agents up to `--to` (`--factor` changes the ratio, `--steps 10,25,50` sets them yourself), holds each level for `--step-duration`, judges every step against your budgets, and stops early once the server has clearly fallen over. A real run against the demo server with a small connection pool, with a 1 s client timeout (`--from 5 --to 40 --step-duration 15s --env MCP_TIMEOUT=1s`):

```text
mcpload capacity steps (p95/p99: all tools/call; errors: all requests):
  Agents     p95     p99  Errors             req/s  Slowest tool p95
       5  306 ms  413 ms  0.31%               42.2  slow 529 ms
      10  513 ms  643 ms  0.41%               78.7  slow 673 ms
      20  849 ms  941 ms  1.38% timeout 8    108.3  slow 966 ms       <- breaks budget: `slow` error rate 6.93% > 1%, all `timeout` (+5 more)
      40  984 ms  997 ms  7.06% timeout 153  148.8  flaky 989 ms      over budget: `slow` error rate 68.39% > 1%, all `timeout` (+8 more)
max sustainable concurrency: 10 agents (budgets broke at 20)
Estimated sustainable capacity: ~11 agents (between 10 (held) and 20 (broke); linear interpolation of `slow` error rate to its 1% budget (the lowest of 6 breached metrics); an estimate, not a measured step)
```

The Errors column shows the error rate and the most frequent error classes. Tool errors from a tool failing within its own error budget (here the demo `flaky` tool) are counted in the rate but not named.

The estimate follows each breached metric in a straight line from the last step that held to the first that broke, and takes the earliest crossing of its budget. `--refine 2` then measures two more steps inside that gap (in a second k6 run) to narrow it. Add `--target 200` to fail the run when the server can't hold 200 agents. Details in [cmd/mcpload/README.md](../../cmd/mcpload/README.md#capacity).

Every option is listed in [scenarios/README.md](../../scenarios/README.md) and [cmd/mcpload/README.md](../../cmd/mcpload/README.md).
