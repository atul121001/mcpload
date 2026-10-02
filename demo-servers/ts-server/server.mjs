// mcpload demo target: official TypeScript MCP SDK over streamable HTTP (stateful).
//
// One image, several personalities (selected by env):
//   ts-healthy   (default)            sessions are cleaned up on DELETE / close / idle
//   ts-leaky     LEAK=1               ~1MB retained per session forever, sessions never removed
//   ts-oauth     REQUIRE_AUTH_URL=... every /mcp request must carry a Bearer token that the
//                                     given RFC 7662 introspection endpoint reports as active
//
// Other env: PORT (3000), FLAKY_RATE (0.1), BIG_BYTES (200000), LEAK_BYTES (1048576),
//            SESSION_IDLE_MS (300000; 0 disables idle reaping), SERVER_NAME.
import { randomUUID, randomFillSync } from 'node:crypto';
import express from 'express';
import client from 'prom-client';
import { z } from 'zod';
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StreamableHTTPServerTransport } from '@modelcontextprotocol/sdk/server/streamableHttp.js';
import { isInitializeRequest } from '@modelcontextprotocol/sdk/types.js';

const PORT = Number(process.env.PORT ?? 3000);
const LEAK = process.env.LEAK === '1';
const REQUIRE_AUTH_URL = process.env.REQUIRE_AUTH_URL || '';
const FLAKY_RATE = Number(process.env.FLAKY_RATE ?? 0.1);
const BIG_BYTES = Number(process.env.BIG_BYTES ?? 200_000);
const LEAK_BYTES = Number(process.env.LEAK_BYTES ?? 1024 * 1024);
const SESSION_IDLE_MS = LEAK ? 0 : Number(process.env.SESSION_IDLE_MS ?? 300_000);
const SERVER_NAME = process.env.SERVER_NAME ?? (LEAK ? 'ts-leaky' : REQUIRE_AUTH_URL ? 'ts-oauth' : 'ts-healthy');
const REPLICA = process.env.HOSTNAME ?? 'local';

const BIG_TEXT = makeBigText(BIG_BYTES);

function makeBigText(n) {
  const line = 'mcpload big payload 0123456789 abcdefghijklmnopqrstuvwxyz ABCDEFGHIJKLMNOPQRSTUVWXYZ\n';
  return line.repeat(Math.ceil(n / line.length)).slice(0, n);
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const text = (t) => ({ content: [{ type: 'text', text: t }] });

// ---- MCP server factory (one McpServer per session, as in the SDK examples) ----
function createMcpServer() {
  const server = new McpServer({ name: SERVER_NAME, version: '0.1.0' });

  server.registerTool('fast', { description: 'Returns immediately.', inputSchema: {} }, async () => text('ok'));

  server.registerTool(
    'slow',
    {
      description: 'Sleeps for `ms` milliseconds (default 300) before returning.',
      inputSchema: { ms: z.number().int().min(0).max(120_000).optional() },
    },
    async ({ ms }) => {
      const d = ms ?? 300;
      await sleep(d);
      return text(`slept ${d}ms`);
    },
  );

  server.registerTool(
    'flaky',
    {
      description: `Returns isError:true with probability \`rate\` (default ${FLAKY_RATE}).`,
      inputSchema: { rate: z.number().min(0).max(1).optional() },
    },
    async ({ rate }) => {
      const p = rate ?? FLAKY_RATE;
      if (Math.random() < p) return { isError: true, content: [{ type: 'text', text: 'flaky: simulated tool failure' }] };
      return text('flaky: ok');
    },
  );

  server.registerTool(
    'big',
    {
      description: `Returns a large text payload (default ${BIG_BYTES} bytes).`,
      inputSchema: { bytes: z.number().int().min(1).max(10_000_000).optional() },
    },
    async ({ bytes }) => text(bytes && bytes !== BIG_BYTES ? makeBigText(bytes) : BIG_TEXT),
  );

  server.registerTool(
    'search',
    {
      description: 'Echoes the query with a short fake result list.',
      inputSchema: { query: z.string(), limit: z.number().int().min(1).max(50).optional() },
    },
    async ({ query, limit }) => {
      const n = limit ?? 5;
      const results = Array.from({ length: n }, (_, i) => ({
        title: `Result ${i + 1} for "${query}"`,
        url: `https://example.com/search/${encodeURIComponent(query)}/${i + 1}`,
        score: Number((1 - i / (n + 1)).toFixed(3)),
      }));
      return text(JSON.stringify({ query, results }));
    },
  );

  return server;
}

// ---- session bookkeeping ----
/** @type {Map<string, {transport: StreamableHTTPServerTransport, server: McpServer, lastSeen: number}>} */
const sessions = new Map();
const leaked = []; // LEAK=1: buffers that are never released

function dropSession(id) {
  if (LEAK) return; // deliberate bug: never forget a session
  const s = sessions.get(id);
  if (!s) return;
  sessions.delete(id);
  s.server.close().catch(() => {});
}

if (SESSION_IDLE_MS > 0) {
  setInterval(() => {
    const cutoff = Date.now() - SESSION_IDLE_MS;
    for (const [id, s] of sessions) if (s.lastSeen < cutoff) { s.transport.close().catch(() => {}); dropSession(id); }
  }, Math.min(SESSION_IDLE_MS, 30_000)).unref();
}

// ---- metrics ----
const register = client.register;
client.collectDefaultMetrics({ register }); // process_resident_memory_bytes, nodejs_heap_size_used_bytes, ...
new client.Gauge({ name: 'nodejs_heap_used_bytes', help: 'V8 heap used (process.memoryUsage().heapUsed).', collect() { this.set(process.memoryUsage().heapUsed); } });
new client.Gauge({ name: 'nodejs_external_memory_bytes_current', help: 'External (Buffer) memory.', collect() { this.set(process.memoryUsage().external); } });
new client.Gauge({ name: 'mcp_active_sessions', help: 'Sessions currently held in the session map.', collect() { this.set(sessions.size); } });
new client.Gauge({ name: 'mcp_leaked_bytes', help: 'Bytes deliberately retained (LEAK=1).', collect() { this.set(leaked.length * LEAK_BYTES); } });
const sessionsCreated = new client.Counter({ name: 'mcp_sessions_created_total', help: 'Sessions created.' });
const sessionNotFound = new client.Counter({ name: 'mcp_session_not_found_total', help: 'Requests carrying an unknown Mcp-Session-Id.' });
const authRejected = new client.Counter({ name: 'mcp_auth_rejected_total', help: 'Requests rejected with 401.' });

// ---- HTTP ----
const app = express();
app.disable('x-powered-by');
app.use((req, res, next) => { res.setHeader('X-Served-By', REPLICA); next(); });

app.get('/healthz', (_req, res) => res.json({ ok: true, name: SERVER_NAME, replica: REPLICA, sessions: sessions.size }));
app.get('/metrics', async (_req, res) => {
  res.setHeader('Content-Type', register.contentType);
  res.end(await register.metrics());
});

function rpcError(res, status, code, message, headers = {}) {
  for (const [k, v] of Object.entries(headers)) res.setHeader(k, v);
  res.status(status).json({ jsonrpc: '2.0', error: { code, message }, id: null });
}

if (REQUIRE_AUTH_URL) {
  app.use('/mcp', async (req, res, next) => {
    const m = /^Bearer\s+(.+)$/i.exec(req.headers.authorization ?? '');
    const challenge = { 'WWW-Authenticate': 'Bearer realm="mcpload-demo", error="invalid_token"' };
    if (!m) { authRejected.inc(); return rpcError(res, 401, -32001, 'Unauthorized: missing bearer token', { 'WWW-Authenticate': 'Bearer realm="mcpload-demo"' }); }
    try {
      const r = await fetch(REQUIRE_AUTH_URL, {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ token: m[1] }),
      });
      const info = r.ok ? await r.json() : { active: false };
      if (!info.active) { authRejected.inc(); return rpcError(res, 401, -32001, 'Unauthorized: invalid or expired token', challenge); }
      next();
    } catch (e) {
      rpcError(res, 503, -32603, `Auth server unavailable: ${e.message}`);
    }
  });
}

app.post('/mcp', express.json({ limit: '4mb' }), async (req, res) => {
  const sid = req.headers['mcp-session-id'];
  try {
    if (sid) {
      const s = sessions.get(sid);
      if (!s) { sessionNotFound.inc(); return rpcError(res, 404, -32001, 'Session not found'); }
      s.lastSeen = Date.now();
      return await s.transport.handleRequest(req, res, req.body);
    }
    if (!isInitializeRequest(req.body)) {
      return rpcError(res, 400, -32000, 'Bad Request: no Mcp-Session-Id header and body is not an initialize request');
    }
    const server = createMcpServer();
    const transport = new StreamableHTTPServerTransport({
      sessionIdGenerator: () => randomUUID(),
      onsessioninitialized: (id) => {
        sessions.set(id, { transport, server, lastSeen: Date.now() });
        sessionsCreated.inc();
        if (LEAK) leaked.push(randomFillSync(Buffer.allocUnsafe(LEAK_BYTES))); // touch every page so RSS grows
      },
      onsessionclosed: (id) => dropSession(id), // DELETE /mcp
    });
    transport.onclose = () => { if (transport.sessionId) dropSession(transport.sessionId); };
    await server.connect(transport);
    await transport.handleRequest(req, res, req.body);
  } catch (e) {
    console.error('POST /mcp failed:', e);
    if (!res.headersSent) rpcError(res, 500, -32603, 'Internal server error');
  }
});

// GET (standalone SSE stream) and DELETE (session termination)
const sessionRequest = async (req, res) => {
  const sid = req.headers['mcp-session-id'];
  if (!sid) return rpcError(res, 400, -32000, 'Bad Request: missing Mcp-Session-Id header');
  const s = sessions.get(sid);
  if (!s) { sessionNotFound.inc(); return rpcError(res, 404, -32001, 'Session not found'); }
  s.lastSeen = Date.now();
  try { await s.transport.handleRequest(req, res); } catch (e) {
    console.error(`${req.method} /mcp failed:`, e);
    if (!res.headersSent) rpcError(res, 500, -32603, 'Internal server error');
  }
};
app.get('/mcp', sessionRequest);
app.delete('/mcp', sessionRequest);

app.listen(PORT, '0.0.0.0', () => {
  console.log(`${SERVER_NAME} listening on :${PORT} (LEAK=${LEAK}, auth=${REQUIRE_AUTH_URL || 'off'}, idle=${SESSION_IDLE_MS}ms)`);
});

for (const sig of ['SIGINT', 'SIGTERM']) process.on(sig, () => process.exit(0));
