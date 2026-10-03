# Use mcpload in CI

[README](../../README.md) · [All docs](../README.md)

The GitHub Action builds everything, runs the test, posts a pass/fail summary with a per-tool table on the pull request, and saves the report as a download.

```yaml
jobs:
  mcp-load:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      pull-requests: write          # only needed for the PR comment
    steps:
      - uses: actions/checkout@v4
      - run: docker compose up -d --build        # start your MCP server
      - uses: atul121001/mcpload-action@v1
        with:
          url: http://localhost:8080/mcp
          wait-ready: 2m                # wait until the server answers an MCP handshake
          scenario: agent-session
          vus: '10'
          duration: 2m
          p95-ms: '800'
          p99-ms: '2000'
          err-rate: '0.01'
          comment-on-pr: 'true'
```

`atul121001/mcpload-action` is the [GitHub Marketplace](https://github.com/atul121001/mcpload-action) entry for this repo's action. To pin an exact mcpload version, use `atul121001/mcpload/action@v0.4.0` instead. All inputs and outputs are documented in [action/action.yml](../../action/action.yml). If your server takes a while to start (loading models, filling connection pools), `wait-ready` (CLI: `--wait-ready 2m`) holds the test until it answers, and fails the step with exit code 2 if it never does. Working examples: [PR gate](../../.github/workflows/example-pr-gate.yml) and [nightly soak](../../.github/workflows/example-nightly-soak.yml).

## Compare with main

Fixed budgets catch a tool that is too slow. They don't catch a tool that got 35% slower in this pull request and is still under budget. Add `baseline-branch: main` and the Action compares the run with the report of the last successful run on `main`, and puts a per-tool Δ table in the job summary and the PR comment:

```yaml
    permissions:
      contents: read
      actions: read                 # to find and download main's report artifact
      pull-requests: write
    # run the same job on: push: branches: [main], so main produces the baseline
      - uses: atul121001/mcpload-action@v1
        with:
          url: http://localhost:8080/mcp
          baseline-branch: main         # or baseline: path/or/url/to/report.json
          fail-on-regression: 'true'    # default; 'false' only warns
          comment-on-pr: 'true'
```

Until `main` has a report (the first run), the comparison is skipped with a notice. The `regressed` output says whether the run regressed. On your machine, compare any two reports with `mcpload compare`, or compare as you run with `mcpload run --baseline main.json`. Here a healthy demo server (port 3001) is the baseline and a server with a too-small connection pool (port 3008) is the change, 10 agents for 30 s each:

```text
$ mcpload compare base.json slow.json
baseline: run 1c961071 · agent-session · 2025-11-25 · started 2026-10-03T05:30:43Z
current:  run ad2ec1ec · agent-session · 2025-11-25 · started 2026-10-03T05:31:59Z
                 baseline   current          Δ       %
search (1,024 → 821 calls)
  p50              2.7 ms     32 ms     +29 ms  +1085%
  p95              5.4 ms    364 ms    +359 ms  +6606%  ⚠
  p99               10 ms    454 ms    +444 ms  +4416%  ⚠
  error rate           0%        0%      0 pts      0%  ·
  req/s              29.2      24.8       -4.4    -15%
slow (206 → 165 calls)
  p50              303 ms    346 ms     +43 ms    +14%
  p95              306 ms    642 ms    +336 ms   +110%  ⚠
  p99              324 ms    959 ms    +635 ms   +196%  ⚠
  error rate           0%        0%      0 pts      0%  ·
  req/s               5.9       5.0       -0.9    -15%
...
run
  error rate         0.7%     0.31%  -0.40 pts    -56%  ·
  connect p95       11 ms     24 ms     +14 ms   +129%  ·
  memory growth   1.1 MiB  10.0 MiB   +8.9 MiB   +784%
    (not judged: needs 2+ min of load after the first 60 s)
Performance regression detected vs baseline run 1c961071:
  - `search` p95 5.4 ms → 364 ms (+6606%, +359 ms)
  ...
```

Exit code 1 on a regression, 0 otherwise. The same server run twice gives "No regression": `search` p95 went from 5.4 ms to 6.4 ms (+17%) and `fast` p99 from 10 ms to 18 ms (+73%), both under the 25 ms floor. CI runners are noisy, so a change only counts when it clears every floor:

- **Latency** (per tool p95 and p99, connect p95): more than +20% (p99: +30%) **and** more than +25 ms. A 5 ms tool going to 8 ms is not a regression.
- **Error rate**: more than +0.5 percentage points, more than +50% of the baseline rate, **and** a two-proportion test says it isn't chance (p < 0.05). 0 of 60 calls failing, then 1 of 60, is not a regression.
- **Enough calls**: a tool is only judged when both runs called it at least 50 times. Tools in only one run are listed as added or removed.
- **Memory**: needs 2+ minutes of load. Fails when RSS grew 5 MiB more than in the baseline, when the leak slope rose by 1 MiB/min, when retained memory after cool-down rose by 5 MiB, or when `memory_leak` went from pass to fail.
- **Fairness**: a different scenario, protocol, load or sampler, or a saturated load generator, gives a warning that the comparison may be unfair.

Change the floors with `--max-p95-increase`, `--max-p99-increase`, `--max-error-increase`, `--min-error-delta`, `--min-delta-ms` and `--min-calls` (in the Action: `extra-args`). Details: [cmd/mcpload/README.md](../../cmd/mcpload/README.md#comparing-with-a-baseline).

## Show that your server is tested

If you run mcpload against your MCP server (for example in CI), you can add this badge to your server's README:

[![soak-tested with mcpload](https://img.shields.io/badge/soak--tested%20with-mcpload-2ea44f)](https://github.com/atul121001/mcpload)

```markdown
[![soak-tested with mcpload](https://img.shields.io/badge/soak--tested%20with-mcpload-2ea44f)](https://github.com/atul121001/mcpload)
```

The badge says that you test with mcpload. It doesn't show a live result, so keep the test running in CI to keep it honest.
