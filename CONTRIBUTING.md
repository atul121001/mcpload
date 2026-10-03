# Contributing to mcpload

Thanks for helping. Issues and pull requests are welcome.

## Before you start

- For anything larger than a small fix, open an issue first so we can agree on the approach.
- Only load-test MCP servers you own or have written permission to test, including while developing.
- By contributing you agree that your contribution is licensed under [Apache-2.0](LICENSE).

## Development setup

See [docs/QUICKSTART.md](docs/QUICKSTART.md) for Docker, Go 1.26+, xk6 and Node 22. Then:

| Area | Check before you push |
|---|---|
| `xk6-mcpload/` | `go vet ./... && go test ./...`, then `xk6 build --with github.com/atul121001/mcpload/xk6-mcpload=. --output ../k6` |
| `cmd/mcpload/` | `go vet ./... && go test ./... && go build .` |
| `scenarios/` | `for f in scenarios/lib/*.js scenarios/*.js; do node --check "$f"; done`, and run the changed scenario against the demo servers |
| `report/` | `npm ci && node validate.mjs examples/healthy.json examples/leaky.json examples/step-load.json`; `node examples/generate.mjs` must leave the examples unchanged |
| `action/` | `node action/summary.mjs report/examples/leaky.json`; lint workflows with [actionlint](https://github.com/rhysd/actionlint) |
| `demo-servers/` | `docker compose up -d --build && ./smoke.sh` |

CI (`.github/workflows/ci.yml`) runs all of these on every pull request.

## Ground rules

- **Calibration.** A new or changed verdict must fire on a known-bad demo target and stay quiet on the known-good one. Add a demo target if none exercises it.
- **The report is a contract.** `report.json` follows [report/schema/README.md](report/schema/README.md): additive changes only within v1; anything breaking needs a `report.v2.json`.
- **Protocol logic lives in `xk6-mcpload/client/`.** Keep it free of k6 imports so it stays unit-testable.
- **No hidden retries** in the wire client: a load tester must see every failure.
- Keep pull requests focused, include tests, and update the relevant README when behaviour or flags change.
- Format Go with `gofmt`. Match the existing style elsewhere.

## Reporting security issues

Please don't open a public issue for a vulnerability. Report it through GitHub private vulnerability reporting instead: open the repository's Security tab and choose "Report a vulnerability". We will respond as soon as we can.

The demo servers in `demo-servers/` are intentionally vulnerable test targets bound to 127.0.0.1; issues in them that only matter if they are exposed to a network are expected.
