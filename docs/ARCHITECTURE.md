# Architecture: mcpload

mcpload is an open-source load and soak testing harness for MCP servers: remote servers over streamable HTTP and, from v0.6.0 (unreleased), local servers over stdio (§1 D8). It is a Go k6 extension. A CLI wraps it: the CLI samples the server's resources during a run, returns leak and regression verdicts, and writes a portable report.

## 1. Key decisions

### D1. A separate implementation
[grafana/xk6-mcp](https://github.com/grafana/xk6-mcp) (AGPL-3.0, experimental, as of Sep 2026) is the existing k6 extension for MCP. mcpload is a separate Apache-2.0 implementation focused on per-tool metrics, soak sampling, the stateless 2026-07-28 protocol and auth. It shares no code with xk6-mcp, and we're happy to contribute upstream where it helps.

### D2. Stack
| Piece | Choice |
|---|---|
| k6 | `go.k6.io/k6/v2` (k6 v2.3 or later). The v2 release changed the module path, so every import uses `/v2`. |
| Go | 1.26 or later (k6 master `go.mod` says `go 1.26.0`, checked 30 Sep 2026) |
| MCP wire format | Plain Go structs in `client/` (no go-sdk dependency). go-sdk v1.8 (which added the 2026-07-28 spec in v1.7.0) is used only as the reference for wire details, and to build the `stateless-2026` demo server. |
| Build | `xk6 build --with github.com/atul121001/mcpload/xk6-mcpload=./xk6-mcpload`. Automatic extension resolution only covers extensions in Grafana's registry, so a custom build is needed until the extension is listed there. |

### D3. Our own thin wire client, not the go-sdk client runtime
A load tester needs things an SDK client hides:
- per-request timing (connect, TTFB, stream duration);
- the raw HTTP status;
- no hidden retries (`MaxRetries`);
- no automatic standalone SSE;
- the ability to inject faults.

So the extension has its own small client in `client/`, and uses plain structs for the JSON types. HTTP goes through the VU's k6 transport and dialer (`vu.State()`), so k6's TLS config, DNS settings and network metrics still apply.

### D4. Support both protocol generations
| | Stateful (2025-06-18, 2025-11-25) | Stateless (2026-07-28) |
|---|---|---|
| Handshake | `initialize` then `notifications/initialized` | none; optional `server/discover` |
| Session | `Mcp-Session-Id` header; DELETE on close | no session ID; servers SHOULD ignore it |
| Per request | JSON-RPC body | JSON-RPC body plus `_meta.io.modelcontextprotocol/{protocolVersion, clientInfo, clientCapabilities}` |
| Headers | `MCP-Protocol-Version` | `MCP-Protocol-Version`, `Mcp-Method`, and `Mcp-Name` (for `tools/call`, `resources/read`, `prompts/get`); `Mcp-Param-*` for parameters marked `x-mcp-header` (planned) |
| Streams | SSE responses to POST (standalone GET stream and `Last-Event-ID` resume are not used) | no GET or DELETE (they return 405); `subscriptions/listen` (planned); MRTR (`InputRequiredResult`) (planned) |
| Server-to-client requests over SSE (sampling, elicitation) | answered while the response stream is read: each request is answered by a POST of the JSON-RPC response (with `Mcp-Session-Id`) from static responders configured on the Client, with an optional delay; unknown methods get `-32601` | forbidden by the spec (MRTR `InputRequiredResult` instead, planned): counted as `unsupported_request`, not answered |
| Cancellation (`cancelAfterMs`, timeouts) | POST `notifications/cancelled {requestId, reason}` with the session headers; the response stream is still read for `cancelWait` to count late responses, then closed | closing the response stream is the cancellation (no notification); late responses cannot be seen |
| Errors of interest | 404 session not found | 400 `-32020 HeaderMismatch` |

`protocol: "auto"` follows the spec's fallback: send a modern request first, and fall back to `initialize` only on a 400 whose body isn't a recognised modern JSON-RPC error.

### D5. True parallel tool calls inside one session
k6 JavaScript runs on a single thread per VU. Real agents fire several `tools/call` requests at once, so `session.callParallel([...])` fans the calls out as goroutines in Go and returns once all have finished. This is the main reason for a Go extension rather than a pure-JS library.

### D6. Server-side leak evidence comes from a sampler outside k6
Memory, file descriptors and session counts live on the server. The `mcpload` CLI starts `k6 run` and samples those resources itself (§6).

### D7. One program for users: the engine is embedded
Users install and run only `mcpload`. Release builds (`go build -tags embedengine`, see `.github/workflows/release.yml`) embed the platform's k6 + xk6-mcpload binary (the "engine") and the `scenarios/` folder with `go:embed`. On first use mcpload extracts them to the user cache folder under a name derived from their SHA-256 (atomic write, hash checked on reuse) and runs the engine from there. Builds without the tag (development, `go test`) embed nothing and fall back to a `k6` in the current folder, next to the mcpload executable, or on `PATH`; `--engine` / `$MCPLOAD_ENGINE` override both. Details in [cmd/mcpload/README.md](../cmd/mcpload/README.md#the-engine). Install scripts (`install.sh`, `install.ps1`) and the Homebrew formula (`packaging/homebrew/`) only ever put `mcpload` on `PATH`.

### D8. stdio transport (v0.6.0, unreleased)
A local MCP server is a subprocess that speaks newline-delimited JSON-RPC on stdin and stdout and logs on stderr. With `command` set instead of `url`, the client (`client/stdio.go`) starts that program directly (never through a shell) for every session, so **one session is one process**, as in desktop clients. The process gets its own process group (Unix) or job object (Windows), so closing the session stops launcher trees such as `npx` and `uvx` too: the client closes stdin, waits up to 2 s, then kills.

Everything above the wire (handshake, tools, resources, prompts, `callParallel`, cancellation, server-to-client requests, metrics) is shared with HTTP; only the exchange of one message differs. Transport-specific rules:
- `protocol: "auto"` sends `initialize` directly (`server/discover` probing is an HTTP rule); explicit versions are honoured.
- No HTTP status (always 0) and no TTFB.
- Cancellation always sends `notifications/cancelled` on stdin; a late response is still observed on stdout for `cancelWait`.
- Server-to-client requests may arrive at any time; they are answered on stdin.
- A stdout line that is not a JSON-RPC message (a log on stdout corrupts the protocol) is skipped and counted (`mcp_stdout_invalid_lines`, verdict `stdout_pollution`).
- When the process exits, every pending request fails with error type `process_exit` (exit code and the tail of stderr in the message).

The CLI passes the target to scenarios as `MCP_COMMAND` (JSON array: program and arguments), `MCP_COMMAND_ENV` (JSON object, added to the inherited environment, which leaves out `MCPLOAD_KEY`, `MCP_TOKEN` and `OAUTH_CLIENT_SECRET`) and `MCP_COMMAND_CWD`; `scenarios/lib/config.js` turns them into the client options. Its default sampler for stdio is `process` (§6). `lb-check`, `version-skew`, `--chaos-restart` and `--calls-url` need an HTTP target and are rejected.

Numbers from a stdio run measure the server process and the host it shares with the load generator, not a network, and are not comparable with HTTP latencies. User guide: [stdio servers](guide/stdio.md).

## 2. Components
```
xk6-mcpload/            Go module → JS import "k6/x/mcpload"
  client/               wire client: POST, JSON/SSE parsing, session state, headers, auth; stdio subprocesses (stdio.go)
  metrics.go            custom metric registration and sample emission
  module.go             RootModule / per-VU ModuleInstance, JS bindings
scenarios/              JS library: agent-session, agent-workflow, burst, soak, long-lived, reconnect-storm, lb-check, isolation, step-load, version-skew, oauth-refresh
cmd/mcpload/            Go CLI: run → sample → analyse → report.json + report.html
demo-servers/           docker compose test targets (§7)
action/                 GitHub Action (composite): build binary, run scenario, gate, upload
docs/
```

The extension follows the standard k6 pattern:
- `modules.Register("k6/x/mcpload", New())`;
- `RootModule.NewModuleInstance(vu)` gives one instance per VU;
- metrics are created at init time through the registry and emitted with `metrics.PushIfNotDone(ctx, state.Samples, ...)`.

## 3. JS API (sketch)
```js
import mcp from 'k6/x/mcpload';

const client = new mcp.Client({
  url: __ENV.MCP_URL,
  // or a local server over stdio (v0.6.0, unreleased): url is then not needed
  // command: 'node', args: ['server.mjs'], env: { K: 'V' }, cwd: './my-server',
  protocol: 'auto',                  // 'auto' | '2026-07-28' | '2025-06-18' | ...
  headers: { 'X-Tenant': 'load' },
  auth: { type: 'oauth', tokenUrl, clientId, clientSecret },   // or { type: 'bearer', token }
  timeout: '30s',
  includePayloads: false,            // accepted, but has no effect yet (planned)
});

export default function () {
  const s = client.connect();        // initialize, or server/discover
  const tools = s.listTools();       // follows pagination
  const results = s.callParallel([
    { name: 'search',  args: { q: 'invoices' } },
    { name: 'get_doc', args: { id: 42 } },
    { kind: 'resource', uri: 'docs://invoices/42' },  // or kind: 'prompt', {name, args}
  ]);
  s.listResources(); s.listResourceTemplates(); s.listPrompts();  // follow pagination
  s.readResource('docs://invoices/42');
  s.getPrompt('summarize', { topic: 'invoices' });
  s.close();                         // DELETE when stateful; no-op when stateless
}
```

## 4. Metrics
Each sample carries the tags `method`, `tool`, `protocol`, `status`, `error_type` and (v0.6.0, unreleased) `transport` (`stdio` or `http`). `resources/read` samples also carry `resource` and `prompts/get` samples `prompt`. `prompt` is the prompt name; `resource` is the server-declared name of the URI (or of the resource template it matches) when the session listed them, else a fallback of scheme, host and at most the first path segment (never the query or user info). Each takes at most 50 distinct values per k6 process; later values are tagged `other`. Details: [xk6-mcpload/README.md](../xk6-mcpload/README.md#metrics).

| Metric | Type | Notes |
|---|---|---|
| `mcp_req_duration` | Trend | full JSON-RPC round trip |
| `mcp_req_ttfb` | Trend | time to the first response byte |
| `mcp_stream_duration` | Trend | SSE response streams |
| `mcp_connect_duration` | Trend | the `initialize` or `server/discover` call |
| `mcp_oauth_refresh_duration` | Trend | token fetch or refresh |
| `mcp_reqs` | Counter | |
| `mcp_errors` | Counter | `error_type` is one of `http`, `jsonrpc`, `tool_iserror`, `timeout`, `session_not_found`, `header_mismatch`, `auth`; over stdio also `process_spawn`, `process_exit` |
| `mcp_tool_error_rate` | Rate | counts `isError: true` results as failures |
| `mcp_sessions_open` | Gauge | client-side open sessions |
| `mcp_server_requests` | Counter | server-to-client requests read from response streams (`method` = the server's method) |
| `mcp_server_request_duration` | Trend | time to answer a server-to-client request |
| `mcp_cancellations` | Counter | cancelled calls, tagged `reason` and `outcome` |
| `mcp_cancel_duration` | Trend | time to send a cancel |
| `mcp_cancel_late_response` | Trend | cancel to a late response (stateful) |
| `mcp_process_spawn_duration` | Trend | stdio: time to start the server process |
| `mcp_processes_open` | Gauge | stdio: server processes running |
| `mcp_stdout_invalid_lines` | Counter | stdio: stdout lines that are not JSON-RPC messages |
| `mcp_process_exits` | Counter | stdio: server process exits, tagged `expected` and `exit_code` |

CI budgets use standard k6 thresholds:
```js
thresholds: {
  'mcp_req_duration{tool:search}': ['p(95)<800', 'p(99)<2000'],
  'mcp_tool_error_rate': ['rate<0.01'],
}
```

## 5. Scenarios (library)
- **agent-session:** connect, list tools, run 1–N rounds of `callParallel`, close. Think time is taken from a distribution.
- **burst:** VUs ramp quickly to simulate many agents starting at once, including an `initialize` flood.
- **soak:** agent-session at constant arrival rate for 30–60 minutes (§6).
- **lb-check:** a session-based flow against more than one replica; checks for `session_not_found` and `header_mismatch`.
- **version-skew:** agent sessions against a load balancer whose replicas run different builds (a rolling deploy halfway through). Every failed request is classified as a fast typed error or a hang; verdict `version_skew`.
- **oauth-refresh:** short-lived tokens with many VUs; measures refresh storms and the 401 rate.
- **long-lived:** one session per agent held for `SESSION_MIN` minutes with think time and pings; sessions that die early (`session_not_found`) and late-vs-early latency within a session (verdict `session_survival`). Soak-style phases, so the leak verdicts apply.
- **reconnect-storm:** agents that reconnect as soon as their session breaks; with `mcpload run --chaos-restart` the CLI restarts the server container mid-run (`docker restart`) and measures recovery (verdict `recovery`). Calls carry a call id in `params._meta["io.mcpload/callId"]`, so a server that records executions (`--calls-url`) shows calls lost or run twice (verdict `call_integrity`).

## 6. Soak and leak method (CLI)
**Samplers** (set with `--sampler`):
- `docker`: the Docker stats API, for local demo servers.
- `prometheus`: scrapes `process_resident_memory_bytes`, heap, open file descriptors and active sessions.
- `process` (stdio, the default there; v0.6.0, unreleased): RSS and open file descriptors of the server processes mcpload started.
- `none`: client-side signals only.

**Run shape:** warm-up (10%), then constant load (30–60 minutes), then a cool-down with no load.

**Verdicts:**
- **Memory leak:** fit a linear regression on RSS over the constant-load window. Flag it when the slope is above the configured MB/min **and** R² ≥ 0.7. Also flag when memory doesn't return to near the post-warm-up baseline during cool-down.
- **Session and connection leaks:** the same method applied to open sessions, file descriptors and connections.
- **Client-side drift:** the slopes of p95 and error rate over time. This is a secondary signal.

**Output:**
- `report.json`: versioned by `schemaVersion`; this is the stable interface for any tooling that consumes results. It holds run metadata, per-tool statistics, threshold results, the time series and verdicts. Tool arguments and results are not stored. The CLI's `--include-payloads` flag is passed through to the scenario (`INCLUDE_PAYLOADS=1`) and recorded as `payloadsIncluded` in the report, but the extension does not yet act on it; storing payloads is planned.
- `report.html`: a static, self-contained single file rendered from `report.json`.

## 7. Demo servers (docker compose)
All demo servers bind to 127.0.0.1 only. Some are intentionally vulnerable or broken; don't expose them to a network.

| Target | Purpose | Expected verdict |
|---|---|---|
| `ts-healthy` | official TypeScript SDK, streamable HTTP; tools: fast, slow, erroring, large payload | pass |
| `ts-leaky` | the same server with a deliberate per-session leak | memory leak flagged |
| `py-healthy` | official Python SDK, stateless mode | pass |
| `lb-stateful` | 2 stateful replicas behind round-robin nginx, **no** sticky sessions | `session_not_found` flagged |
| `stateless-2026` | go-sdk v1.8 server, 2026-07-28 protocol, same LB | pass |
| `mock-oauth` | token issuer with 30-second expiry | refresh storm measured |
| `ts-server --stdio` | the TS image over stdio (v0.6.0, unreleased), personas `normal`, `blocking`, `leaky`, `noisy` | pass; `tool_isolation`, memory growth per process and `stdout_pollution` expected on the bad personas |

**Calibration rule:** before a verdict ships, it must be true on the bad target and false on the good one.

## Sources (checked 30 Sep 2026)
- grafana/xk6-mcp repo and `go.mod`: https://github.com/grafana/xk6-mcp
- k6 v2.0.0 release (module path change): https://github.com/grafana/k6/releases/tag/v2.0.0
- k6 JS extension guide: https://grafana.com/docs/k6/latest/extensions/create/javascript-extensions/
- xk6: https://github.com/grafana/xk6
- MCP Go SDK: https://github.com/modelcontextprotocol/go-sdk (releases, pkg.go.dev)
- MCP 2026-07-28 streamable HTTP: https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http
