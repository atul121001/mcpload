# mcpload demo servers

> [!WARNING]
> **Local-only, intentionally vulnerable test targets. Do not expose them.**
> Some targets leak memory on purpose, the LB targets are misconfigured on purpose, and `mock-oauth`
> is not a real authorization server (its client credentials are public). `docker-compose.yml` binds
> every port to `127.0.0.1` and disables automatic restarts; keep it that way and never run these on a
> shared or internet-facing host.

Known-good and known-bad MCP targets for calibrating mcpload verdicts
(see `docs/ARCHITECTURE.md` §7: every verdict must fire on the bad target and stay quiet on the good one).

Every MCP endpoint is `http://localhost:<port>/mcp` (streamable HTTP). All servers expose the same five tools; the TypeScript servers add two that send server-to-client requests:

| Tool | Arguments | Behaviour |
|---|---|---|
| `fast` | none | returns `ok` immediately |
| `slow` | `ms` (int, default 300) | sleeps `ms`, then returns |
| `flaky` | `rate` (0..1, default `FLAKY_RATE` = 0.1) | returns `isError: true` with probability `rate` |
| `big` | `bytes` (default `BIG_BYTES` = 200000) | returns a ~200 KB text block |
| `search` | `query` (string, required), `limit` (default 5) | echoes the query with a fake result list (JSON text) |
| `sample_llm` | `prompt` (string), `maxTokens` (default 100) | TS servers only. Sends `sampling/createMessage` to the client on the call's SSE stream and returns the reply; `isError: true` when the client did not declare `sampling` |
| `elicit_input` | `message` (string) | TS servers only. Sends `elicitation/create` (form: optional `confirm` boolean, `note` string) and returns the action and content; `isError: true` when the client did not declare `elicitation` |

The default tool mix (`scenarios/lib/tools.js`) only calls the first five, so `sample_llm` and `elicit_input` run
only when named in `TOOL_MIX` (with `SAMPLING` / `ELICITATION` set) or by
[`xk6-mcpload/examples/client-requests.js`](../xk6-mcpload/examples/client-requests.js).

## Targets

| Port | Target | Implementation | Protocol | Expected verdict |
|---|---|---|---|---|
| 3001 | `ts-healthy` | `@modelcontextprotocol/sdk` 1.31 + Express, `StreamableHTTPServerTransport`, stateful (UUID session ids), sessions removed on DELETE / close / 5 min idle | 2025-11-25 (stateful) | **pass** |
| 3002 | `ts-leaky` | same image, `LEAK=1`: keeps a 1 MB buffer per session forever and never removes sessions (even after DELETE) | 2025-11-25 (stateful) | **memory leak flagged** (RSS and `mcp_active_sessions` grow linearly with sessions) |
| 3003 | `py-healthy` | `mcp` 2.2.0 (Python), `MCPServer` (FastMCP's name in mcp 2.x), `stateless_http=True`, uvicorn | legacy stateless (no session) **and** 2026-07-28 | **pass** |
| 3004 | `lb-stateful` | nginx round-robin over 2 × `ts-healthy` (`lb-stateful-a`, `lb-stateful-b`), **no** sticky sessions | 2025-11-25 (stateful) | **`session_not_found` flagged** (~50% of in-session requests get HTTP 404 / `-32001 Session not found`) |
| 3005 | `stateless-2026` | nginx round-robin over 2 × Go server, `github.com/modelcontextprotocol/go-sdk` v1.8.0, `StreamableHTTPOptions{Stateless: true}` | 2026-07-28 (stateless, `server/discover`, `Mcp-Method`/`Mcp-Name`) and legacy | **pass** (requests spread 50/50, no errors) |
| 3006 | `mock-oauth` | dependency-free Node token server: `POST /token` (client_credentials, form-encoded, Basic or body creds `mcpload:secret`), `POST /introspect` (RFC 7662). Tokens are opaque and expire after **30 s** | OAuth 2.0 | n/a (auth server) |
| 3008 | `ts-pooled` | same image, `POOL_SIZE=2`: every tool call must hold one of 2 process-wide slots (like one small DB connection pool shared by all tools), so `fast` and `search` queue behind `slow` | 2025-11-25 (stateful) | **`tool_isolation` flagged** with `--scenario isolation` (ts-healthy passes it) |
| 3007 | `ts-oauth` | `ts-healthy` with `REQUIRE_AUTH_URL=http://mock-oauth:3000/introspect`: every `/mcp` request must carry a Bearer token that introspects as active, otherwise **401** | 2025-11-25 (stateful) | **refresh storm measured** (clients must refresh every 30 s) |

Side endpoints:
- `GET /metrics` (Prometheus text) on 3001, 3002, 3003, 3007, mock-oauth (3006) and each Go replica.
  - TS: `process_resident_memory_bytes`, `nodejs_heap_used_bytes`, `mcp_active_sessions`, `mcp_leaked_bytes`, `mcp_sessions_created_total`, `mcp_session_not_found_total`, `mcp_auth_rejected_total`, plus prom-client default metrics.
  - Python / Go: `process_resident_memory_bytes`, `mcp_active_sessions` (always 0); Go also `go_memstats_heap_alloc_bytes`, `go_goroutines`.
  - mock-oauth: `oauth_tokens_issued_total`, `oauth_introspect_total{active}`, `oauth_live_tokens`.
  - Behind the two nginx LBs (3004, 3005), `/metrics` reaches one replica at a time (round-robin). Use `docker stats` for per-replica memory.
- `GET /healthz` on every server.
- Responses carry `X-Served-By: <replica hostname>`; the LBs also add `X-Upstream: <ip:port>`.

Memory limits (`docker stats` shows them): TS / Python 256 MiB, **ts-leaky 512 MiB** (so ~400 leaked sessions fit before the OOM killer stops it; with `restart: "no"` it stays down until you `docker compose up -d ts-leaky` again), Go 128 MiB, nginx and mock-oauth 64 MiB.

## Start / stop

```bash
cd demo-servers
docker compose up -d --build      # builds 4 images: ts-server, py-server, go-server, mock-oauth
docker compose ps
./smoke.sh                        # asserting smoke test of every target, exits non-zero on failure (or: ./smoke.sh lb-stateful)
docker compose restart ts-leaky   # reset the leak between runs
docker compose down
```

No local Node, Python or Go is needed: everything builds inside Docker (`node:22-alpine`,
`python:3.12-slim`, `golang:1.26-alpine` → `distroless/static`). The Go server's `go.mod` / `go.sum`
(module `github.com/atul121001/mcpload/demo-servers/go-server`) are committed; the image only runs `go mod download`.

Tunables (env in `docker-compose.yml`): `FLAKY_RATE`, `BIG_BYTES`, `LEAK`, `LEAK_BYTES` (1 MiB),
`SESSION_IDLE_MS` (TS, 300000; 0 = off), `REQUIRE_AUTH_URL`, `TOKEN_TTL_SECONDS` (30), `CLIENTS` (`id:secret,...`),
`JSON_RESPONSE=1` (Python/Go: reply `application/json` instead of SSE).

## curl examples

Every POST needs `Content-Type: application/json` and `Accept: application/json, text/event-stream`.
The TS servers reply as SSE (`event: message` / `data: {...}`); parse the `data:` line.

### Stateful targets (3001, 3002, 3004, 3007)

```bash
A='Accept: application/json, text/event-stream'; C='Content-Type: application/json'; U=http://localhost:3001/mcp

# 1. initialize -> the session id comes back in the Mcp-Session-Id response header
SID=$(curl -s -D - -o /dev/null -H "$C" -H "$A" $U \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"curl","version":"1.0"}}}' \
  | grep -i '^mcp-session-id:' | awk '{print $2}' | tr -d '\r')

# 2. notifications/initialized -> 202
curl -s -o /dev/null -w '%{http_code}\n' -H "$C" -H "$A" -H "Mcp-Session-Id: $SID" -H 'MCP-Protocol-Version: 2025-11-25' $U \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'

# 3. tools/list, tools/call
curl -s -H "$C" -H "$A" -H "Mcp-Session-Id: $SID" -H 'MCP-Protocol-Version: 2025-11-25' $U \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
curl -s -H "$C" -H "$A" -H "Mcp-Session-Id: $SID" -H 'MCP-Protocol-Version: 2025-11-25' $U \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search","arguments":{"query":"mcp"}}}'

# 4. close the session -> 200 (a later request with this id -> 404 "Session not found")
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE -H "Mcp-Session-Id: $SID" -H 'MCP-Protocol-Version: 2025-11-25' $U
```

Error contract (TS): unknown session id → **404** `{"error":{"code":-32001,"message":"Session not found"}}`;
no session id on a non-initialize POST → **400** `-32000`.

### ts-oauth (3007)

```bash
TOK=$(curl -s -u mcpload:secret -d grant_type=client_credentials http://localhost:3006/token \
  | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
# (form creds also work: -d client_id=mcpload -d client_secret=secret)
# then the stateful sequence above against http://localhost:3007/mcp with -H "Authorization: Bearer $TOK"
# missing / expired (>30 s) token -> 401 + WWW-Authenticate: Bearer ... error="invalid_token"
```

### Stateless targets (3003, 3005)

Legacy stateless: no initialize, no session, just call.

```bash
curl -s -H "$C" -H "$A" -H 'MCP-Protocol-Version: 2025-11-25' http://localhost:3005/mcp \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"query":"mcp"}}}'
```

2026-07-28: per-request `_meta` plus `Mcp-Method` / `Mcp-Name` headers.

```bash
M='"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"curl","version":"1.0"}}'
curl -s -H "$C" -H "$A" -H 'MCP-Protocol-Version: 2026-07-28' -H 'Mcp-Method: server/discover' http://localhost:3005/mcp \
  -d '{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{'"$M"'}}'
curl -s -H "$C" -H "$A" -H 'MCP-Protocol-Version: 2026-07-28' -H 'Mcp-Method: tools/call' -H 'Mcp-Name: search' http://localhost:3005/mcp \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"query":"mcp"},'"$M"'}}'
```

On 3005, GET/DELETE `/mcp` return 405, and a header/body mismatch returns 400 `-32020` (HeaderMismatch).
On 3003, `server/discover` advertises only `2026-07-28`, but legacy stateless calls (above) also work.

## Verified behaviour (30 Sep 2026)

- `lb-stateful`: 1 initialize then 40 × `tools/call` on the same session → **20 × 200, 20 × 404**.
  `notifications/initialized` right after initialize already hits the other replica (404).
- `stateless-2026`: 40 × 2026-07-28 `tools/call` → 20 served by `stateless-2026-a`, 20 by `-b`, all 200.
- `ts-leaky` vs `ts-healthy`, 300 × (initialize + DELETE) each:

  | | before | after | `mcp_active_sessions` after |
  |---|---|---|---|
  | ts-healthy (`docker stats`) | 29.3 MiB | 31.2 MiB | 0 |
  | ts-leaky (`docker stats`) | 30.2 MiB | **351 MiB** | **301** (`mcp_leaked_bytes` 315621376) |
- `ts-oauth`: no token → 401; fresh token → 200; the same token after 31 s → 401; a refreshed token → 200.
- `flaky` at the default 0.1: 12/100 errors (Python), 7/100 (TS). `big` is ~202 KB on the wire. `slow` takes ~0.31 s.

## Implementation notes

SDK statements below describe the pinned versions as of 30 Sep 2026 and may change in later releases.

- **nginx `worker_processes 1`.** With the default `auto`, each worker keeps its own round-robin counter.
  Fresh client connections then all land on replica A, which hides the missing stickiness. A single worker
  makes the alternation deterministic (exactly 50% 404s for one client). `proxy_next_upstream off` stops
  nginx from retrying on the other replica.
- **Python: `MCPServer` (formerly `FastMCP`).** As of `mcp` 2.2.0 (30 Sep 2026), the Python SDK exposes the
  high-level server as `MCPServer` (`from mcp.server.mcpserver import MCPServer`); the 1.x
  `mcp.server.fastmcp` import path isn't available there. Tools use `structured_output=False`; otherwise
  the server also mirrors the text into `structuredContent` and `big` roughly doubles to ~400 KB.
- **TS targets speak 2025-11-25 only.** As of `@modelcontextprotocol/sdk` 1.31.0 (30 Sep 2026), the
  TypeScript SDK doesn't yet support 2026-07-28, so the TS targets are legacy stateful only.
- **Go: 2026-07-28 via `Stateless: true`.** As of `github.com/modelcontextprotocol/go-sdk` v1.8.0
  (30 Sep 2026), the Go SDK serves 2026-07-28 requests on handlers created with
  `StreamableHTTPOptions{Stateless: true}`, which is what `stateless-2026` uses.
- `prom-client` 15.1.3 prints an npm deprecation notice (pointing to `@prometheus-io/client`); it works
  fine for these targets.
- `nodejs_heap_used_bytes` is a custom gauge (`process.memoryUsage().heapUsed`). The prom-client default
  name is `nodejs_heap_size_used_bytes`, which is also exported. The leak lives in Buffers (off-heap),
  so it shows in RSS, not in heap-used.
