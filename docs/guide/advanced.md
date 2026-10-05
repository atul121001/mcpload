# Advanced: custom scenarios, metrics, repo layout

[README](../../README.md) · [All docs](../README.md)

mcpload's load engine is [k6](https://k6.io) with an MCP extension ([`xk6-mcpload`](../../xk6-mcpload/README.md)) built into the `mcpload` binary. You never install or run k6 yourself, but if you want full control you can script traffic in k6's JavaScript API. How the pieces fit together is in [ARCHITECTURE.md](../ARCHITECTURE.md).

## Write your own scenarios (JavaScript)

mcpload's load engine is [k6](https://k6.io) with an MCP extension built in, so you can script any traffic pattern as a k6 script and run it with `mcpload run --scenario my-test.js --url ...`:

```js
import mcp from 'k6/x/mcpload';

const client = new mcp.Client({
  url: __ENV.MCP_URL,
  protocol: 'auto',                       // or '2026-07-28', '2025-11-25', ...
  auth: { type: 'bearer', token: __ENV.MCP_TOKEN },
});

export const options = {
  vus: 10, duration: '1m',
  thresholds: { 'mcp_req_duration{tool:search}': ['p(95)<800'], 'mcp_tool_error_rate': ['rate<0.01'] },
};

export default function () {
  const s = client.connect();
  s.listTools();
  s.callParallel([{ name: 'search', args: { query: 'invoices' } }, { name: 'fast', args: {} }]);
  s.close();
}
```

Full API: [xk6-mcpload/README.md](../../xk6-mcpload/README.md).

## Metrics

| Metric | Type | What it measures |
|---|---|---|
| `mcp_req_duration` | Trend | each JSON-RPC round trip (tagged by `method`, `tool`, `status`, `error_type`; `resources/read` also by `resource`, `prompts/get` by `prompt`) |
| `mcp_req_ttfb` | Trend | time to the first response byte |
| `mcp_stream_duration` | Trend | SSE responses: headers to matching event |
| `mcp_connect_duration` | Trend | the whole `connect()` (`initialize` or `server/discover`) |
| `mcp_oauth_refresh_duration` | Trend | OAuth token fetch |
| `mcp_reqs` | Counter | requests |
| `mcp_errors` | Counter | errors by `error_type`: `http`, `jsonrpc`, `tool_iserror`, `timeout`, `session_not_found`, `header_mismatch`, `auth` |
| `mcp_tool_error_rate` | Rate | failed `tools/call` (including `isError: true`) |
| `mcp_sessions_open` | Gauge | client-side open sessions |
| `mcp_server_requests` | Counter | server-to-client requests (sampling, elicitation, ...) answered inside response streams, by `method`; unexpected ones count in `mcp_errors` as `unsupported_request` |
| `mcp_server_request_duration` | Trend | time to answer a server-to-client request (includes the simulated `delayMs`) |
| `mcp_cancellations` | Counter | cancelled calls by `reason` (`client`: `cancelAfterMs`; `timeout`) and `outcome` (`cancelled`, `late_response`, `send_failed`, or `completed` when the call beat its cancel deadline). Cancelled calls carry `error_type` `cancelled` in `mcp_reqs` but are not counted in `mcp_errors` |
| `mcp_cancel_duration` | Trend | time to send a cancel (the `notifications/cancelled` POST, or closing the stream on 2026-07-28) |
| `mcp_cancel_late_response` | Trend | cancel sent to a late response arriving anyway (stateful only) |

Report format: [report/schema/README.md](../../report/schema/README.md).

## Repository layout

| Path | What |
|---|---|
| [`xk6-mcpload/`](../../xk6-mcpload/README.md) | the k6 extension that speaks MCP (Go) |
| [`cmd/mcpload/`](../../cmd/mcpload/README.md) | the `mcpload` command: runs k6, samples the server, computes verdicts, writes the report |
| [`scenarios/`](../../scenarios/README.md) | ready-made test scenarios |
| [`report/`](../../report/schema/README.md) | report format, validator and HTML report |
| [`demo-servers/`](../../demo-servers/README.md) | healthy and deliberately broken demo servers (local-only, intentionally vulnerable) |
| [`action/`](../../action/action.yml) | the GitHub Action |

