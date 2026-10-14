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
| `slow` | `ms` (int, default 300) | sleeps `ms`, then returns. TS servers stop sleeping when the call is cancelled (`notifications/cancelled`), except `ts-ignore-cancel` |
| `flaky` | `rate` (0..1, default `FLAKY_RATE` = 0.1) | returns `isError: true` with probability `rate` |
| `big` | `bytes` (default `BIG_BYTES` = 200000) | returns a ~200 KB text block |
| `search` | `query` (string, required), `limit` (default 5) | echoes the query with a fake result list (JSON text) |
| `sample_llm` | `prompt` (string), `maxTokens` (default 100) | TS servers only. Sends `sampling/createMessage` to the client on the call's SSE stream and returns the reply; `isError: true` when the client did not declare `sampling` |
| `elicit_input` | `message` (string) | TS servers only. Sends `elicitation/create` (form: optional `confirm` boolean, `note` string) and returns the action and content; `isError: true` when the client did not declare `elicitation` |

The TS servers also serve resources and a prompt (the Python and Go servers don't):

| Kind | Name | URI / arguments | Returns |
|---|---|---|---|
| resource | `readme` | `demo://docs/readme` | a short Markdown text (`text/markdown`) |
| resource | `changelog` | `demo://docs/changelog` | a short Markdown text (`text/markdown`) |
| resource template | `item` | `demo://items/{id}` (any id) | `{"id","name","price"}` as JSON text (`application/json`); not listed in `resources/list` |
| prompt | `summarize` | `topic` (required), `style` (optional) | one user message asking for a summary of `topic`; `-32602` without `topic` |

Scenarios read and get these only with `RESOURCE_READ_RATIO` / `PROMPT_GET_RATIO` set (see `scenarios/README.md`).

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
| 3009 | `skew` | nginx round-robin over two replicas on **different builds** (a rolling deploy caught halfway): `skew-old` = the TS image (stateful 2025-11-25 only), `skew-new` = the Go image with `STATELESS_ONLY=1` (2026-07-28 only: `initialize` or an older `Mcp-Protocol-Version` gets HTTP 400 `-32022` Unsupported protocol version, `data.supported: ["2026-07-28"]`) and `NEW_TOOL=1` (also lists `new_tool`) | mixed | **`version_skew` warns** with `--scenario version-skew` (every mismatch fails fast: HTTP 400 from the old build, -32022 from the new one, `Tool new_tool not found` with `TOOLS_CACHE_TTL`) |
| 3010 | `skew-hang` | the same, but the old replica is `skew-hang-old` (`HANG_UNKNOWN=1`): a request it can't serve (no session and not `initialize`, e.g. a 2026-07-28 request; an unknown session; a `tools/call` for a tool it doesn't have) is accepted and never answered | mixed | **`version_skew` fails** (requests hang until the client timeout) |
| 3007 | `ts-oauth` | `ts-healthy` with `REQUIRE_AUTH_URL=http://mock-oauth:3000/introspect`: every `/mcp` request must carry a Bearer token that introspects as active, otherwise **401** | 2025-11-25 (stateful) | **refresh storm measured** (clients must refresh every 30 s) |
| 3011 | `ts-ignore-cancel` | same image, `IGNORE_CANCEL=1`: `notifications/cancelled` is recorded but ignored, so a cancelled call keeps running and still sends its (late) response | 2025-11-25 (stateful) | **`cancellation` flagged** with `CANCEL_RATE` set and `--sampler prometheus` (ts-healthy passes it) |

Side endpoints:
- `GET /metrics` (Prometheus text) on 3001, 3002, 3003, 3007, 3008, 3011, mock-oauth (3006) and each Go replica.
  - TS: `process_resident_memory_bytes`, `nodejs_heap_used_bytes`, `mcp_active_sessions`, `mcp_leaked_bytes`, `mcp_sessions_created_total`, `mcp_session_not_found_total`, `mcp_auth_rejected_total`, `mcp_cancelled_total{tool}` (calls cancelled while running), `mcp_work_after_cancel_seconds{tool}` (histogram: how long a cancelled call kept running after its cancel arrived; ~0 when honoured), `mcp_cancelled_inflight` (cancelled calls still running), plus prom-client default metrics.
  - Python / Go: `process_resident_memory_bytes`, `mcp_active_sessions` (always 0); Go also `go_memstats_heap_alloc_bytes`, `go_goroutines`.
  - mock-oauth: `oauth_tokens_issued_total`, `oauth_introspect_total{active}`, `oauth_live_tokens`.
  - Behind the nginx LBs (3004, 3005, 3009, 3010), `/metrics` reaches one replica at a time (round-robin). Use `docker stats` for per-replica memory.
- `GET /healthz` on every server.
- Responses carry `X-Served-By: <replica hostname>`; the LBs also add `X-Upstream: <ip:port>`.

Memory limits (`docker stats` shows them): TS / Python 256 MiB, **ts-leaky 512 MiB** (so ~400 leaked sessions fit before the OOM killer stops it; with `restart: "no"` it stays down until you `docker compose up -d ts-leaky` again), Go 128 MiB, nginx and mock-oauth 64 MiB.

## Start / stop

```bash
cd demo-servers
docker compose up -d --build      # builds ts-server, py-server, go-server, mock-oauth (+ the :skew tags of ts-server and go-server)
docker compose ps
./smoke.sh                        # asserting smoke test of every target, exits non-zero on failure (or: ./smoke.sh lb-stateful)
docker compose restart ts-leaky   # reset the leak between runs
docker compose down
```

No local Node, Python or Go is needed: everything builds inside Docker (`node:22-alpine`,
`python:3.12-slim`, `golang:1.26-alpine` → `distroless/static`). The Go server's `go.mod` / `go.sum`
(module `github.com/atul121001/mcpload/demo-servers/go-server`) are committed; the image only runs `go mod download`.

Tunables (env in `docker-compose.yml`): `FLAKY_RATE`, `BIG_BYTES`, `LEAK`, `LEAK_BYTES` (1 MiB),
`SESSION_IDLE_MS` (TS, 300000; 0 = off), `REQUIRE_AUTH_URL`, `IGNORE_CANCEL=1` (TS), `TOKEN_TTL_SECONDS` (30), `CLIENTS` (`id:secret,...`),
`JSON_RESPONSE=1` (Python/Go: reply `application/json` instead of SSE), `SERVER_NAME`, `NEW_TOOL=1` and `STATELESS_ONLY=1` (Go),
`HANG_UNKNOWN=1` (TS).

The skew targets use their own image tags (`mcpload-demo/ts-server:skew`, `mcpload-demo/go-server:skew`), so
`docker compose up -d --build skew skew-hang` never rebuilds the images the other targets run.

### Without a clone: `mcpload demo` and the published images

Each release publishes these servers to GHCR, for `linux/amd64` and `linux/arm64`, tagged with the release version (`0.6.0`) and `latest`:

| Image | Built from | Used by |
|---|---|---|
| `ghcr.io/atul121001/mcpload-demo-ts` | `ts-server/` | ts-healthy, ts-leaky, ts-oauth, ts-pooled, ts-ignore-cancel, the lb-stateful and skew-old replicas |
| `ghcr.io/atul121001/mcpload-demo-py` | `py-server/` | py-healthy |
| `ghcr.io/atul121001/mcpload-demo-go` | `go-server/` | the stateless-2026 replicas, skew-new |
| `ghcr.io/atul121001/mcpload-demo-oauth` | `mock-oauth/` | mock-oauth |
| `ghcr.io/atul121001/mcpload-demo-nginx` | `nginx/` (stock `nginx:1.29-alpine` with the four configs in `/etc/nginx/mcpload/`) | lb-stateful, stateless-2026, skew, skew-hang |

`mcpload demo up` runs them with a compose file embedded in the mcpload binary
([`cmd/mcpload/internal/demo/compose.yml`](../cmd/mcpload/internal/demo/compose.yml)): the same services, ports, env,
memory limits and project name (`mcpload-demo`) as `docker-compose.yml` here, with images instead of build
contexts and the nginx configs baked in instead of mounted. It pulls the tag that matches the CLI's version
(`latest` for development builds) and waits until every target answers `/healthz`.

```bash
mcpload demo up        # start, wait, print the URLs and what each one demonstrates
mcpload demo status    # containers and which targets answer
mcpload demo logs -f ts-leaky
mcpload demo down
mcpload demo config    # print the compose file it runs
```

`--base-port 13001` moves the targets to 13001-13011 and `--project-name` changes the container names (both are
needed to run a second copy next to this one). `MCPLOAD_DEMO_IMAGE_PREFIX` and `MCPLOAD_DEMO_TAG` select other images,
e.g. ones built locally:

```bash
docker compose build
docker build -t mcpload-local/demo-nginx:dev nginx
for s in ts py go; do docker tag mcpload-demo/$s-server:local mcpload-local/demo-$s:dev; done
docker tag mcpload-demo/mock-oauth:local mcpload-local/demo-oauth:dev
MCPLOAD_DEMO_IMAGE_PREFIX=mcpload-local/demo MCPLOAD_DEMO_TAG=dev mcpload demo up
```

When you change a service, port, env var or memory limit here, make the same change in the embedded compose file;
`go test ./internal/demo` (in `cmd/mcpload`) fails until the two match.

### Call tracking and chaos restarts (TS image, off in compose)

`TRACK_CALLS=1` makes the TS server record every `tools/call` that carries a call id in `params._meta["io.mcpload/callId"]` (sent by `scenarios/reconnect-storm.js`). The id is recorded when the tool's handler starts, the point where a non-idempotent tool would act. Records are appended to `CALL_LOG` (default `/tmp/mcpload-calls.log`, inside the container, which survives `docker restart`) and reloaded on start. `GET /calls?prefix=<p>` returns `{"executions": {"<call id>": <times run>}}`, and `/metrics` adds `mcp_tool_executions_total` and `mcp_tool_duplicate_executions_total`. `DEDUPE=atomic` skips a call id that already ran (an idempotency key); `DEDUPE=racy` makes the same check but records the id only after an `await` (`DEDUPE_RACE_MS`, 20), so two concurrent calls with one id both pass it: the non-atomic duplicate check.

None of the compose services set these, and `mcpload run --chaos-restart` restarts a container, so run a private copy for chaos tests rather than a shared target:

```sh
docker build -t mcpload-chaos/ts-server:local demo-servers/ts-server
docker run -d --rm --name mcpload-chaos-ts -p 127.0.0.1:3019:3000 -e TRACK_CALLS=1 mcpload-chaos/ts-server:local
./mcpload run --scenario reconnect-storm --url http://localhost:3019/mcp --vus 10 --duration 60s \
  --chaos-restart 20s --chaos-container mcpload-chaos-ts --calls-url http://localhost:3019/calls
docker stop mcpload-chaos-ts
```

### stdio mode (TS image)

*Since v0.6.0.* `--stdio` runs the same tools, resources and prompt over the MCP stdio transport (the SDK's `StdioServerTransport`): newline-delimited JSON-RPC on stdin and stdout, one session per process, no HTTP port and no `/metrics`. Every log goes to stderr. Pick a persona with `--persona <name>` (or `--persona=<name>`) or `PERSONA=<name>`; `LEAK=1` also selects `leaky`. An unknown persona exits with code 2.

| Persona | `serverInfo.name` | Behaviour | What mcpload should show |
|---|---|---|---|
| `normal` (default) | `ts-stdio` | `slow` sleeps asynchronously, so concurrent calls on one process overlap | pass |
| `blocking` | `ts-stdio-blocking` | synchronous CPU work: `slow` busy-waits `ms` (and ignores cancellation), every other tool busy-waits `BLOCK_MS` (10). A `fast` call sent right after a 300 ms `slow` call on the same process waits ~300 ms | head-of-line blocking: `tool_isolation` with `--scenario isolation` |
| `leaky` | `ts-stdio-leaky` | every `tools/call` retains `LEAK_CALL_BYTES` (102400) forever | the process's RSS grows with the calls it served (`--scenario long-lived`, `process` sampler) |
| `noisy` | `ts-stdio-noisy` | writes a line to **stdout** at start and on every `NOISY_EVERY`-th (1) `tools/call`: lines that are not JSON-RPC | `stdout_pollution` |

`TRACK_CALLS`, `DEDUPE`, `POOL_SIZE`, `IGNORE_CANCEL`, `FLAKY_RATE` and `BIG_BYTES` work as over HTTP; `REQUIRE_AUTH_URL`, `HANG_UNKNOWN`, `PORT` and `SESSION_IDLE_MS` are HTTP-only. The process exits when its stdin closes. HTTP mode (no `--stdio`) is unchanged.

```sh
cd demo-servers/ts-server && npm ci && cd ../..        # local Node 22
./mcpload run --command "node demo-servers/ts-server/server.mjs --stdio"
./mcpload run --command "node demo-servers/ts-server/server.mjs --stdio --persona blocking" --scenario isolation
./mcpload run --command "node demo-servers/ts-server/server.mjs --stdio" --command-env PERSONA=noisy

# or from the image (its ENTRYPOINT is node server.mjs, so arguments reach the server)
docker build -t mcpload-demo/ts-server:local demo-servers/ts-server
./mcpload run --command "docker run -i --rm -e PERSONA=leaky mcpload-demo/ts-server:local --stdio" --scenario long-lived
```

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

- `skew` (3 Oct 2026): `server/discover` alternates between `skew-old` (400 `-32000` "no Mcp-Session-Id header") and `skew-new` (200, `supportedVersions: ["2026-07-28"]`); `initialize` alternates between `skew-old` (200, session id) and `skew-new` (400 `-32022`); in a `skew-old` session, `tools/call new_tool` returns `isError: true` "MCP error -32602: Tool new_tool not found".
- `skew-hang`: `server/discover` on `skew-hang-old` gets no answer (curl `-m 3` gives up after 3.0 s); on `skew-new` it returns 200 in 0.2 s.

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
