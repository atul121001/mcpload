# Architecture: mcpload

mcpload is an open-source load and soak testing harness for remote MCP servers that use streamable HTTP. It is a Go k6 extension. A CLI wraps it: the CLI samples the server's resources during a run, returns leak and regression verdicts, and writes a portable report.

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
| Server-to-client requests over SSE (sampling, elicitation) | (planned) | (planned) |
| Errors of interest | 404 session not found | 400 `-32020 HeaderMismatch` |

`protocol: "auto"` follows the spec's fallback: send a modern request first, and fall back to `initialize` only on a 400 whose body isn't a recognised modern JSON-RPC error.

### D5. True parallel tool calls inside one session
k6 JavaScript runs on a single thread per VU. Real agents fire several `tools/call` requests at once, so `session.callParallel([...])` fans the calls out as goroutines in Go and returns once all have finished. This is the main reason for a Go extension rather than a pure-JS library.

### D6. Server-side leak evidence comes from a sampler outside k6
Memory, file descriptors and session counts live on the server. The `mcpload` CLI starts `k6 run` and samples those resources itself (§6).

## 2. Components
```
xk6-mcpload/            Go module → JS import "k6/x/mcpload"
  client/               wire client: POST, JSON/SSE parsing, session state, headers, auth
  metrics.go            custom metric registration and sample emission
  module.go             RootModule / per-VU ModuleInstance, JS bindings
scenarios/              JS library: agent-session, burst, soak, lb-check, oauth-refresh
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
  ]);
  s.close();                         // DELETE when stateful; no-op when stateless
}
```

## 4. Metrics
Each sample carries the tags `method`, `tool`, `protocol`, `status` and `error_type`.

| Metric | Type | Notes |
|---|---|---|
| `mcp_req_duration` | Trend | full JSON-RPC round trip |
| `mcp_req_ttfb` | Trend | time to the first response byte |
| `mcp_stream_duration` | Trend | SSE response streams |
| `mcp_connect_duration` | Trend | the `initialize` or `server/discover` call |
| `mcp_oauth_refresh_duration` | Trend | token fetch or refresh |
| `mcp_reqs` | Counter | |
| `mcp_errors` | Counter | `error_type` is one of `http`, `jsonrpc`, `tool_iserror`, `timeout`, `session_not_found`, `header_mismatch`, `auth` |
| `mcp_tool_error_rate` | Rate | counts `isError: true` results as failures |
| `mcp_sessions_open` | Gauge | client-side open sessions |

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
- **oauth-refresh:** short-lived tokens with many VUs; measures refresh storms and the 401 rate.

## 6. Soak and leak method (CLI)
**Samplers** (set with `--sampler`):
- `docker`: the Docker stats API, for local demo servers.
- `prometheus`: scrapes `process_resident_memory_bytes`, heap, open file descriptors and active sessions.
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

**Calibration rule:** before a verdict ships, it must be true on the bad target and false on the good one.

## Sources (checked 30 Sep 2026)
- grafana/xk6-mcp repo and `go.mod`: https://github.com/grafana/xk6-mcp
- k6 v2.0.0 release (module path change): https://github.com/grafana/k6/releases/tag/v2.0.0
- k6 JS extension guide: https://grafana.com/docs/k6/latest/extensions/create/javascript-extensions/
- xk6: https://github.com/grafana/xk6
- MCP Go SDK: https://github.com/modelcontextprotocol/go-sdk (releases, pkg.go.dev)
- MCP 2026-07-28 streamable HTTP: https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http
