# Quick start

From a fresh clone to an HTML report in about five minutes (most of it is the first Docker and Go build).

## Prerequisites

| Tool | Version | Install |
|---|---|---|
| Docker (with Compose v2) | any recent | Docker Desktop on Windows/macOS, Docker Engine on Linux |
| Go | 1.26 or later | <https://go.dev/dl/> (Windows: `winget install GoLang.Go`) |
| xk6 | latest | `go install go.k6.io/xk6@latest` (Windows: `winget install GrafanaLabs.xk6` also works) |
| Node.js | 22 (optional) | only for `report/` tooling |

Make sure `$(go env GOPATH)/bin` (Windows: `%USERPROFILE%\go\bin`) is on your `PATH`, so `xk6` is found.

> Windows: keep the repo outside cloud-synced folders such as OneDrive. Sync interferes with Go build caches and `node_modules`.

## 1. Start the demo MCP servers

The demo servers are known-good and known-bad targets on ports 3001–3007. They bind to 127.0.0.1 only and some are intentionally vulnerable, so don't expose them to a network (see [demo-servers/README.md](../demo-servers/README.md)).

Linux / macOS:

```bash
cd demo-servers
docker compose up -d --build
./smoke.sh ts-healthy       # optional curl smoke test
cd ..
```

Windows (PowerShell):

```powershell
cd demo-servers
docker compose up -d --build
docker compose ps
cd ..
```

## 2. Build k6 (with xk6-mcpload) and the mcpload CLI

Linux / macOS:

```bash
go install go.k6.io/xk6@latest
xk6 build --with github.com/atul121001/mcpload/xk6-mcpload=./xk6-mcpload --output ./k6
(cd cmd/mcpload && go build -o ../../mcpload .)
./k6 version && ./mcpload version
```

Windows (PowerShell):

```powershell
go install go.k6.io/xk6@latest
xk6 build --with github.com/atul121001/mcpload/xk6-mcpload=./xk6-mcpload --output .\k6.exe
Push-Location cmd\mcpload; go build -o ..\..\mcpload.exe .; Pop-Location
.\k6.exe version; .\mcpload.exe version
```

## 3. Run a scenario

A one-minute agent-session test against the healthy TypeScript server, sampling its container with `docker stats`.

Linux / macOS:

```bash
./mcpload run --scenario scenarios/agent-session.js --url http://localhost:3001/mcp \
  --k6 ./k6 --sampler docker --container mcpload-demo-ts-healthy-1 \
  --vus 10 --duration 1m --out report.json --html report.html
echo "exit code: $?"
```

Windows (PowerShell):

```powershell
.\mcpload.exe run --scenario scenarios\agent-session.js --url http://localhost:3001/mcp `
  --k6 .\k6.exe --sampler docker --container mcpload-demo-ts-healthy-1 `
  --vus 10 --duration 1m --out report.json --html report.html
"exit code: $LASTEXITCODE"
```

Exit code `0` means pass, `1` means a verdict or budget failed, `2` means an error (bad flags, k6 could not start, target unreachable).

## 4. Open the report

```bash
xdg-open report.html     # Linux
open report.html         # macOS
```

```powershell
Start-Process report.html
```

`report.json` is the machine-readable version ([schema](../report/schema/README.md)). Check one with `mcpload validate report.json`, or re-render it with `mcpload render report.json report.html`.

## 5. See the verdicts fire

| Try | Expected |
|---|---|
| `--url http://localhost:3004/mcp --scenario scenarios/lb-check.js` | **fail**: `session_not_found` (LB without sticky sessions) |
| `--url http://localhost:3005/mcp --scenario scenarios/lb-check.js --protocol 2026-07-28` | pass: stateless 2026-07-28 behind the same LB |
| `--url http://localhost:3002/mcp --scenario scenarios/soak.js --sampler prometheus --prom-url http://localhost:3002/metrics --soak-min 30` | **fail**: `memory_leak` and `session_leak` (about 40 minutes with warm-up and cool-down) |
| `--url http://localhost:3007/mcp --scenario scenarios/oauth-refresh.js --env OAUTH_TOKEN_URL=http://localhost:3006/token --env OAUTH_CLIENT_ID=mcpload --env OAUTH_CLIENT_SECRET=secret` | OAuth refresh storm measured (`mcp_oauth_refresh_duration`). `mcpload` / `secret` are the demo mock-oauth credentials. |

Reset the leaky server between runs with `docker compose -f demo-servers/docker-compose.yml restart ts-leaky`.

## Budgets and scenario knobs

Scenario settings are env vars passed with `--env K=V` (repeatable). The most common:

| Var | Default | Meaning |
|---|---|---|
| `P95_MS`, `P99_MS`, `ERR_RATE` | `800`, `2000`, `0.01` | default per-tool budgets |
| `TOOL_BUDGETS` | `{"flaky":{"errRate":0.2}}` | per-tool overrides |
| `TOOL_MIX` | demo tools | JSON weights of which tools to call |
| `TOOL_ARGS` | `{"search":{...}}` | JSON arguments per tool |
| `MCP_TOKEN` | – | static bearer token |

The full list is in [scenarios/README.md](../scenarios/README.md).

## Upload a report (optional)

If you collect reports in a service of your own, `mcpload upload` POSTs a `report.json` to `{url}/api/v1/runs`, with the API key from `--key` or `$MCPLOAD_KEY`:

```bash
export MCPLOAD_KEY=<api key>
./mcpload upload --url https://<your-endpoint> report.json
```

```powershell
$env:MCPLOAD_KEY = "<api key>"
.\mcpload.exe upload --url https://<your-endpoint> report.json
```

## In CI

Use the GitHub Action (see the [README](../README.md#github-action) and [action/action.yml](../action/action.yml)). The
[PR gate example](../.github/workflows/example-pr-gate.yml) starts the demo servers in the job and gates on them.

## Clean up

```bash
docker compose -f demo-servers/docker-compose.yml down -v
```
