# Quick start

From a fresh clone to an HTML report in about five minutes (most of it is the first Docker build).

> **No clone needed to just try it.** With a release of mcpload installed, `mcpload demo up` starts the demo servers from published images and `mcpload demo down` stops them; see [Install and try mcpload](guide/getting-started.md#try-it-in-5-minutes). There is also a Docker image, `ghcr.io/atul121001/mcpload` ([Run with Docker](guide/testing-your-server.md#run-with-docker)). This guide builds everything from source.

## Prerequisites

| Tool | Version | Install |
|---|---|---|
| Docker (with Compose v2) | any recent | Docker Desktop on Windows/macOS, Docker Engine on Linux |
| mcpload | latest release | one command, see [step 2](#2-install-mcpload) |
| Go | 1.26 or later (optional) | only to build mcpload from source: <https://go.dev/dl/> (Windows: `winget install GoLang.Go`) |
| Node.js | 22 (optional) | only for `report/` tooling |

> Windows: keep the repo outside cloud-synced folders such as OneDrive. Sync interferes with Go build caches and `node_modules`.

## 1. Start the demo MCP servers

The demo servers are known-good and known-bad targets on ports 3001–3011. They bind to 127.0.0.1 only and some are intentionally vulnerable, so don't expose them to a network (see [demo-servers/README.md](../demo-servers/README.md)).

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

If you don't need to change the demo servers, `mcpload demo up` (once mcpload is installed, step 2) starts the same servers from the published images without building them, waits until they answer, and lists what each one demonstrates. Both ways use the project name `mcpload-demo`, so the container names (`mcpload-demo-ts-healthy-1`, ...) are the same; run one or the other.

## 2. Install mcpload

mcpload is a single program. The install command downloads the build for your computer, verifies its SHA-256 checksum, and puts `mcpload` on your `PATH` (no admin rights needed; run it again to upgrade).

Linux / macOS:

```bash
curl -fsSL https://raw.githubusercontent.com/atul121001/mcpload/main/install.sh | sh
mcpload version
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/atul121001/mcpload/main/install.ps1 | iex
mcpload version
```

Other terminals that were already open on Windows only see `mcpload` after you restart them. With Homebrew: `brew install atul121001/tap/mcpload`.

`mcpload version` also shows the engine and the scenarios folder mcpload will use, so you can check an install from any folder.

<details>
<summary>Settings for the install scripts</summary>

| Variable | Meaning |
|---|---|
| `MCPLOAD_VERSION` | release to install, e.g. `v0.5.0` (default: latest) |
| `MCPLOAD_INSTALL_DIR` | where releases are unpacked (default: `~/.mcpload`, Windows: `%LOCALAPPDATA%\mcpload`) |
| `MCPLOAD_BIN_DIR` | Linux/macOS: where the `mcpload` link goes (default: `~/.local/bin`, or `/usr/local/bin` if it is writable and `~/.local/bin` is not on `PATH`) |
| `MCPLOAD_NO_PATH=1` | unpack only; don't touch `PATH` |

For example: `curl -fsSL https://raw.githubusercontent.com/atul121001/mcpload/main/install.sh | MCPLOAD_VERSION=v0.5.0 sh`.

</details>

<details>
<summary>Build from source instead</summary>

You need Go 1.26+ and xk6 (`go install go.k6.io/xk6@latest`; make sure `$(go env GOPATH)/bin` is on your `PATH`). mcpload's load engine is k6 with the [`xk6-mcpload`](../xk6-mcpload/README.md) extension, embedded into the binary with the `embedengine` build tag:

Linux / macOS:

```bash
mkdir -p cmd/mcpload/internal/engine/bin
xk6 build --with github.com/atul121001/mcpload/xk6-mcpload=./xk6-mcpload --output cmd/mcpload/internal/engine/bin/engine
cp -r scenarios cmd/mcpload/internal/engine/bin/scenarios
(cd cmd/mcpload && go build -tags embedengine -o ../../mcpload .)
./mcpload version
```

Windows (PowerShell):

```powershell
New-Item -ItemType Directory -Force cmd\mcpload\internal\engine\bin | Out-Null
xk6 build --with github.com/atul121001/mcpload/xk6-mcpload=./xk6-mcpload --output cmd\mcpload\internal\engine\bin\engine.exe
Move-Item -Force cmd\mcpload\internal\engine\bin\engine.exe cmd\mcpload\internal\engine\bin\engine
Copy-Item -Recurse -Force scenarios cmd\mcpload\internal\engine\bin\scenarios
Push-Location cmd\mcpload; go build -tags embedengine -o ..\..\mcpload.exe .; Pop-Location
.\mcpload.exe version
```

Use `./mcpload` (Windows: `.\mcpload.exe`) in place of `mcpload` below. A plain `go build` without the tag also works for development; it then looks for a `k6` built with xk6-mcpload in the current folder, next to mcpload, or on `PATH` (or pass `--engine <path>`).

</details>

## 3. Run a scenario

A one-minute agent-session test against the healthy TypeScript server, sampling its container with `docker stats`.

Linux / macOS:

```bash
mcpload run --url http://localhost:3001/mcp \
  --sampler docker --container mcpload-demo-ts-healthy-1 \
  --vus 10 --duration 1m --out report.json --html report.html
echo "exit code: $?"
```

Windows (PowerShell):

```powershell
mcpload run --url http://localhost:3001/mcp `
  --sampler docker --container mcpload-demo-ts-healthy-1 `
  --vus 10 --duration 1m --out report.json --html report.html
"exit code: $LASTEXITCODE"
```

With no `--scenario`, mcpload runs the built-in `agent-session` scenario. Name another built-in one with `--scenario soak` (or `lb-check`, `isolation`, ...), or pass a path to your own script. A `scenarios/` folder in the current directory (such as the repo's) takes precedence over the built-in copies.

Exit code `0` means pass, `1` means a verdict or budget failed, `2` means an error (bad flags, the test could not start, target unreachable).

Besides the leak, drift, session and budget checks, mcpload also reports a `generator` verdict about the load generator itself. It warns when it dropped more than 1% of the planned iterations (fails above 10%), or when the load generator went over 90% CPU at its busiest, in which case the measured latency may include its own overhead. If it fires, lower the load or run mcpload on another machine before trusting the numbers. The full list of verdicts is in [Scenarios and verdicts](guide/scenarios-and-verdicts.md#reading-the-result).

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
| `--url http://localhost:3008/mcp --scenario isolation` | **fail**: `tool_isolation` (every tool shares 2 slots, so fast tools wait behind `slow`) |
| `--url http://localhost:3001/mcp --scenario isolation` | pass: same tools, no shared pool |
| `--url http://localhost:3002/mcp --scenario scenarios/soak.js --sampler prometheus --prom-url http://localhost:3002/metrics --soak-min 30` | **fail**: `memory_leak` and `session_leak` (about 40 minutes with warm-up and cool-down) |
| `--url http://localhost:3007/mcp --scenario scenarios/oauth-refresh.js --env OAUTH_TOKEN_URL=http://localhost:3006/token --env OAUTH_CLIENT_ID=mcpload --env OAUTH_CLIENT_SECRET=secret` | OAuth refresh storm measured (`mcp_oauth_refresh_duration`). `mcpload` / `secret` are the demo mock-oauth credentials. |

Leak verdicts (`memory_leak`, `session_leak`, `fd_leak`) are only judged on soak runs, which have a cool-down, and need at least 2 minutes of steady load; on other runs they show as skipped. For real leak hunting use 10 minutes or more of steady load with a sampler. Shorter soaks only reliably catch leaks of about 2 MiB/min or more. `soak.js` takes `RATE` in new sessions per second, and fractions such as `--env RATE=0.05` are fine.

Reset the leaky server between runs with `docker restart mcpload-demo-ts-leaky-1`.

## Budgets and scenario knobs

Scenario settings are env vars passed with `--env K=V` (repeatable). The most common:

| Var | Default | Meaning |
|---|---|---|
| `P95_MS`, `P99_MS`, `ERR_RATE` | `800`, `2000`, `0.01` | default budget, applied to every tool that has no entry in `TOOL_BUDGETS` |
| `TOOL_BUDGETS` | none (demo servers: `{"flaky":{"errRate":0.2}}`) | per-tool overrides, e.g. `{"slow":{"p95":1500,"p99":2500}}` |
| `TOOL_MIX` | every tool the server lists | JSON weights of which tools to call. Names the server doesn't list produce a warning and a failed check. |
| `TOOL_ARGS` | sample arguments (demo servers: `{"search":{...}}`) | JSON arguments per tool |
| `MCP_TOKEN` | – | static bearer token |

Per-tool p50/p95/p99 are computed over successful calls; failed calls count toward the error rate. Defaults marked "demo servers" only apply when the target is one of the bundled demo servers.

JSON values need care on Windows. Windows PowerShell 5.1 drops the double quotes inside a native command's arguments, so escape them with a backslash:

```powershell
# Windows PowerShell 5.1
mcpload run --url http://localhost:3001/mcp --scenario agent-session --env 'TOOL_MIX={\"search\":5}'
```

PowerShell 7.3 or newer (`pwsh`) passes arguments as written, so use the same form as bash there: `--env 'TOOL_MIX={"search":5}'`. The backslash form would break JSON under PowerShell 7.3+.

The full list is in [scenarios/README.md](../scenarios/README.md).

## Getting trustworthy results

- Run mcpload on a different machine from the server, or at least on separate CPU cores. Contention on a shared host skews latency.
- Start with a few VUs and a short run, then raise the load step by step.
- Use soaks of 10 minutes or more, with `--sampler docker` or `--sampler prometheus`, when hunting leaks.
- Watch the `generator` verdict. A warning or fail there means part of what you measured is the load generator, not the server.

## Upload a report (optional)

If you collect reports in a service of your own, `mcpload upload` POSTs a `report.json` to `{url}/api/v1/runs`, with the API key from `--key` or `$MCPLOAD_KEY`:

```bash
export MCPLOAD_KEY=<api key>
mcpload upload --url https://<your-endpoint> report.json
```

```powershell
$env:MCPLOAD_KEY = "<api key>"
mcpload upload --url https://<your-endpoint> report.json
```

## In CI

Use the GitHub Action (see [Use mcpload in CI](guide/ci.md) and [action/action.yml](../action/action.yml)). The
[PR gate example](../.github/workflows/example-pr-gate.yml) starts the demo servers in the job and gates on them.

## Clean up

```bash
docker compose -f demo-servers/docker-compose.yml down -v
```

If you started them with `mcpload demo up`, use `mcpload demo down`.
