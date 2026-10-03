# xk6-mcpload

k6 extension (`import mcp from 'k6/x/mcpload'`) for load testing remote MCP servers over streamable HTTP.
Apache-2.0. Targets `go.k6.io/k6/v2` (v2.3.0) and Go 1.26+.

## Build

```bash
go install go.k6.io/xk6@latest          # or: winget install GrafanaLabs.xk6
# from the repo root
xk6 build --with github.com/atul121001/mcpload/xk6-mcpload=./xk6-mcpload --output ./k6.exe
./k6.exe run -e MCP_URL=http://localhost:3001/mcp xk6-mcpload/examples/smoke.js
```

Or build straight from a release, without cloning:

```bash
xk6 build v2.3.0 --with github.com/atul121001/mcpload/xk6-mcpload@v0.2.0
```

Tests: `cd xk6-mcpload && go test ./... && go vet ./...`

## Compatibility

| xk6-mcpload | k6 | Go |
|---|---|---|
| v0.1.x, v0.2.x | v2.3.0 (`go.k6.io/k6/v2`) | 1.26+ |

k6 v1 and earlier are not supported. Extension releases are tagged `xk6-mcpload/vX.Y.Z` (Go submodule tags) alongside the mcpload `vX.Y.Z` release tags.

## JS API

```js
import mcp from 'k6/x/mcpload';

// Construct in the init context (one Client per VU).
const client = new mcp.Client({
  url: 'http://localhost:3001/mcp',
  protocol: 'auto',          // 'auto' or a revision date: '2026-07-28' | '2025-11-25' | '2025-06-18' | ...
                             // anything else (e.g. 'latest') throws when the Client is constructed
  headers: { 'X-Tenant': 'load' },
  auth: { type: 'bearer', token: '...' },
  //  or { type: 'oauth', tokenUrl, clientId, clientSecret, scope?, audience?, authStyle?: 'basic'|'post',
  //       failureBackoff?: '1s',  // negative cache for failed token fetches; 0 disables
  //       timeout?: '10s' }       // per token fetch; default: the client's `timeout`
  timeout: '30s',            // per HTTP exchange, and per token wait/fetch; string or milliseconds
  includePayloads: false,    // reserved; payloads are never attached to metric samples
  // answer server-to-client requests (stateful protocols; see below); each one set is declared in initialize
  sampling: { response: { role: 'assistant', content: { type: 'text', text: 'ok' }, model: 'mcpload-mock', stopReason: 'endTurn' },
              delayMs: 200 },                                   // or `true` for this default answer
  elicitation: { action: 'accept', content: { confirm: true }, delayMs: 100 },  // action: 'accept' | 'decline' | 'cancel'
  roots: { roots: [{ uri: 'file:///work', name: 'work' }] },
  // advanced: fallbackVersion ('2025-11-25'), discover (true), rememberProtocol (true),
  //           clientInfo {name, version}, capabilities {}, servedByHeader ('X-Served-By')
});

export default function () {
  const s = client.connect();     // throws on failure (Error with .type, .status, .code, .servedBy)
  s.protocol; s.sessionId;        // sessionId is '' in stateless mode
  s.servedBy;                     // servedByHeader of the handshake response ('' if absent or nothing was sent)
  const tools = s.listTools();    // follows nextCursor; [{name, description, inputSchema}]; throws on failure
  const r = s.callTool('search', { q: 'x' });
  // never throws: { isError, content, structuredContent?, durationMs, servedBy?, error?: {type, message, status?, code?, servedBy?} }
  const rs = s.callParallel([{ name: 'a', args: {} }, { name: 'b', args: {} }]); // goroutines, input order
  // optional params._meta, e.g. a call id the server can record (scenarios/reconnect-storm.js):
  s.callTool('search', { q: 'x' }, { meta: { 'io.mcpload/callId': 'r1-3-17' } }); // callParallel: {name, args, meta}
  s.ping();                       // throws on failure
  s.close();                      // DELETE in stateful mode; no-op on the wire in stateless mode
}
```

`error.type` (and the `error_type` tag) is one of `http`, `jsonrpc`, `tool_iserror`, `timeout`,
`session_not_found`, `header_mismatch`, `auth`. A result with `isError: true` has `isError: true` **and**
`error.type === 'tool_iserror'`. `mcp_errors` can also carry `unsupported_request` (see below).

### Server-to-client requests

A stateful server (`2025-xx`) may send JSON-RPC requests on the SSE stream of a client request, typically a
`tools/call` whose tool needs an LLM completion or user input. While reading the stream the client answers each one by
POSTing a JSON-RPC response to the MCP endpoint (with `Mcp-Session-Id` and `MCP-Protocol-Version`; the server
replies `202`), then keeps reading until the original response arrives. Answers run in goroutines, so several
requests on one stream (or across `callParallel`) are answered concurrently; the call returns only after its answers
are done.

- `sampling/createMessage`, `elicitation/create`, `roots/list`: answered with the configured `sampling`,
  `elicitation` or `roots` option after its `delayMs` (simulated LLM or human time; a duration string also works).
  Each option that is set declares the capability (`sampling: {}`, `elicitation: {}` = form mode, `roots: {}`) in
  `initialize`, unless `capabilities` already has that key. `error: {code, message}` answers with a JSON-RPC error
  instead (e.g. `{code: -1, message: 'User rejected sampling request'}`). Defaults for `true`: a mock text reply
  from model `mcpload-mock`; `{action: 'accept', content: {}}`; `{roots: []}`.
- `ping`: always answered with `{}`.
- anything else, or one of the above without its option: answered with `-32601` (method not found) and counted as
  `unsupported_request`.
- **Stateless** (`2026-07-28`): the spec forbids servers to send requests on response streams and clients to POST
  responses (sampling, elicitation and roots go through MRTR `InputRequiredResult`, not implemented yet). A request
  seen there is not answered, is counted as `unsupported_request`, and the client keeps waiting for the response.
  The capabilities are not declared in `_meta` either.

Answers are static, fixed when the Client is constructed: they are sent from Go goroutines while a call or
`callParallel` is in flight, where the VU's JS runtime must not be touched, so JS callback responders are not
supported. `mcp_req_duration` of the call includes the time spent answering. Example:
[examples/client-requests.js](examples/client-requests.js) against the TS demo server's `sample_llm` and
`elicit_input` tools.

### Protocol behaviour

- **Stateful** (`2025-xx`): `initialize` → `Mcp-Session-Id` → `notifications/initialized`; later requests
  carry `Mcp-Session-Id` and `MCP-Protocol-Version`; `close()` sends DELETE (405 is accepted).
  A 404 on a request that carried a session id is `session_not_found`. If `initialize` succeeded but
  `notifications/initialized` fails, the new session is DELETEd (best effort, recorded as a `DELETE` request)
  before `connect()` throws, so failing handshakes do not leak server sessions.
- **Stateless** (`2026-07-28`): no handshake; every request carries `params._meta`
  `io.modelcontextprotocol/{protocolVersion,clientInfo,clientCapabilities}` and the headers
  `MCP-Protocol-Version`, `Mcp-Method`, `Mcp-Name` (tools/call, resources/read, prompts/get; values with
  non-ASCII/control characters or leading/trailing whitespace, and plain values that themselves look like
  `=?base64?…?=`, are sent as `=?base64?…?=`). `connect()` calls `server/discover` unless `discover: false`. `ping()` sends
  `server/discover` because the new protocol removed `ping`. JSON-RPC `-32020` is `header_mismatch`.
- **auto**: `connect()` sends a stateless `server/discover`; on a non-modern error (e.g. HTTP 400 `-32000` from
  the TypeScript SDK (@modelcontextprotocol/sdk 1.31.0, Sep 2026), 404/405, or `-32601`) it falls back to `initialize` with `fallbackVersion`. Timeouts, 5xx, auth
  errors, `-32020` and `-32021` do not fall back. A `-32022` (UnsupportedProtocolVersion) whose
  `error.data.supported` lists a stateless version (`>= 2026-07-28`) is a modern server: `server/discover` is
  retried once with the newest such version, and only if none is offered does it fall back to `initialize`. A probe
  answered with a fallback-triggering (or retry-triggering) error is recorded in `mcp_reqs`/`mcp_req_duration`
  (with its real `status` tag) but not in `mcp_errors`. If the fallback `initialize` is itself rejected with a `-32022`
  that offers a stateless version (replicas on different builds behind one load balancer: the probe reached an old
  build, the handshake a new one), `server/discover` is tried once more with that version; the rejected `initialize`
  stays counted as an error.
- **servedBy**: the value of the `servedByHeader` response header (default `X-Served-By`; e.g. nginx's
  `X-Upstream`) on the session (handshake), on each tool result and its `error`, and on thrown errors. Empty when
  the header is absent or no response arrived (timeout). It is not a metric tag. `scenarios/version-skew.js` uses it.
- **rememberProtocol** (default `true`, only affects `auto`): the protocol resolved by the first *successful* auto
  connect is cached process-wide, keyed by `url` + `protocol` + `fallbackVersion`, and shared by every VU and
  Client in the k6 process. Later connects skip the `server/discover` probe: a stateless (`2026-07-28`) server
  gets no request at all on `connect()` (capabilities and server info come from the cache, and the
  `mcp_connect_duration` sample has no `method` tag), and a stateful server goes straight to `initialize`. Failed
  connects are never cached, so a VU whose first connect fails re-probes. If a cached stateful version is later
  rejected as a protocol error, the entry is dropped and the next connect probes again; likewise, when a
  request on a session that reused a cached stateless resolution gets a "legacy server" answer (e.g. HTTP
  400/404/405, `-32601` or `-32022`; not an ordinary JSON-RPC error with HTTP 200), the entry is dropped so the
  next connect re-probes (that request still fails). Concurrent first
  connects (before anything is cached) each probe. `rememberProtocol: false` probes on every connect.
- **OAuth** (client_credentials): one token cache per credential set shared by all VUs, with a single in-flight
  fetch. The fetch runs detached from the VU that triggered it (an iteration being cut off does not fail other
  VUs waiting for the same token) and is bounded by the oauth `timeout` (default: the client's `timeout`); each
  caller waits at most the client's `timeout`, and a fetch or wait that runs out of time fails with error type
  `timeout`. In the last 20% of a token's lifetime (max 60 s early) the refresh runs in the background and callers
  keep using the still-valid token without waiting. A 401 from the MCP server drops the cached token
  (the next request fetches a new one). A failed token fetch (4xx/5xx, bad response or network error) is cached
  for `failureBackoff` (default `1s`; a duration string or milliseconds; `0` disables): every connect or request in
  that window, from any VU, fails immediately with the cached `auth` (or `timeout`) error (its message says it was cached, and no
  `mcp_oauth_refresh_duration` sample is emitted), so a wrong secret or a down IdP costs about one token request
  per window instead of one per connect. While a still-valid token exists, a failed early refresh keeps using it
  and waits out the window before retrying. No request is ever retried.

## Metrics

Tags: `method`, `tool`, `protocol`, `status`, `error_type` (empty tags are omitted) plus the VU's tags.
A successful request has **no** `error_type` tag; a failed one carries it on all of its samples, including
`mcp_req_duration`, so success-only latency can be selected by the tag's absence.

| Metric | Type | Notes |
|---|---|---|
| `mcp_req_duration` | Trend (time) | per HTTP exchange (JSON-RPC round trip; `DELETE` for session close) |
| `mcp_req_ttfb` | Trend (time) | time to first response byte |
| `mcp_stream_duration` | Trend (time) | SSE responses: response headers → matching event |
| `mcp_connect_duration` | Trend (time) | whole `connect()` (`method` = `initialize` or `server/discover`) |
| `mcp_oauth_refresh_duration` | Trend (time) | token fetch (`method=oauth/token`) |
| `mcp_reqs` | Counter | |
| `mcp_errors` | Counter | tagged with `error_type` (server-to-client requests add `unsupported_request`, or the answer POST's error type) |
| `mcp_tool_error_rate` | Rate | per `tools/call`; any failure (including `isError`) counts |
| `mcp_sessions_open` | Gauge | process-wide open sessions (see below) |
| `mcp_server_requests` | Counter | server-to-client requests read from response streams; `method` is the server's method (`sampling/createMessage`, ...), `tool` the call whose stream carried it, `status` the answer POST's status. Not counted in `mcp_reqs` |
| `mcp_server_request_duration` | Trend (time) | request read off the stream → answer POST completed (includes `delayMs`); not emitted when nothing was sent |

`mcp_sessions_open` is the number of sessions currently open in this k6 process — every VU, Client and scenario
together: +1 on a successful `connect()`, −1 on the first `close()` (stateless sessions count too). Each change is
pushed with the new total, in order, tagged only with the test-wide `options.tags` (no VU, scenario or group
tags), so the gauge's last value is the current count. `close()` after the VU's context has ended still
decrements the count; its own sample is dropped by k6, and the next push from any VU carries the correct value.
Sessions a script never closes stay counted.

## Layout

- `client/` — wire client (pure Go, no k6 imports): JSON/SSE parsing, session state, headers, auth, timings.
- `metrics.go` — metric registration (`vu.InitEnv().Registry`) and sample emission (`metrics.PushIfNotDone`).
- `module.go` — `modules.Register("k6/x/mcpload", …)`, per-VU instance, JS bindings.
