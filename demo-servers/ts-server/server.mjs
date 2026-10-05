// mcpload demo target: official TypeScript MCP SDK over streamable HTTP (stateful).
//
// One image, several personalities (selected by env):
//   ts-healthy   (default)            sessions are cleaned up on DELETE / close / idle
//   ts-leaky     LEAK=1               ~1MB retained per session forever, sessions never removed
//   ts-oauth     REQUIRE_AUTH_URL=... every /mcp request must carry a Bearer token that the
//                                     given RFC 7662 introspection endpoint reports as active
//   ts-pooled    POOL_SIZE=N          every tool call holds one of N shared slots (like one small DB
//                                     connection pool for all tools): fast tools queue behind slow ones
//   skew-hang-old HANG_UNKNOWN=1      a request this build can't serve (no session and not initialize, e.g. a
//                                     2026-07-28 stateless request; an unknown session id; a tools/call for a
//                                     tool it doesn't have) is accepted and never answered, instead of a 400/404
//                                     or a "tool not found" error: the hang of a rolling deploy gone wrong
//   ts-ignore-cancel IGNORE_CANCEL=1  notifications/cancelled is recorded but ignored: cancelled calls keep
//                                     running and still send their (late) response
//
// Cancellation (all personalities): `slow` stops sleeping when its request is cancelled (the SDK aborts the
// handler's signal on notifications/cancelled and then sends no response). /metrics counts cancels
// (mcp_cancelled_total), how long each cancelled call still ran (mcp_work_after_cancel_seconds) and cancelled
// calls still running (mcp_cancelled_inflight).
//
// Other env: PORT (3000), FLAKY_RATE (0.1), BIG_BYTES (200000), LEAK_BYTES (1048576),
//            SESSION_IDLE_MS (300000; 0 disables idle reaping), SERVER_NAME.
//
// Call tracking (TRACK_CALLS=1, off by default): every tools/call that carries params._meta["io.mcpload/callId"]
// is recorded when its handler starts (the point where a non-idempotent tool would act), appended to CALL_LOG
// (default /tmp/mcpload-calls.log; the container's own filesystem survives `docker restart`) and reloaded on
// start. GET /calls?prefix=<p> returns {executions: {callId: count}}; /metrics adds mcp_tool_executions_total and
// mcp_tool_duplicate_executions_total. DEDUPE=atomic skips a call id that already ran (an idempotency key);
// DEDUPE=racy does the same check but records the id only after an await (DEDUPE_RACE_MS, 20), so two concurrent
// calls with one id can both pass it: the non-atomic duplicate check.
import { appendFileSync, existsSync, readFileSync } from 'node:fs';
import { randomUUID, randomFillSync } from 'node:crypto';
import express from 'express';
import client from 'prom-client';
import { z } from 'zod';
import { McpServer, ResourceTemplate } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StreamableHTTPServerTransport } from '@modelcontextprotocol/sdk/server/streamableHttp.js';
import { isInitializeRequest, CancelledNotificationSchema } from '@modelcontextprotocol/sdk/types.js';

const PORT = Number(process.env.PORT ?? 3000);
const LEAK = process.env.LEAK === '1';
const REQUIRE_AUTH_URL = process.env.REQUIRE_AUTH_URL || '';
const FLAKY_RATE = Number(process.env.FLAKY_RATE ?? 0.1);
const BIG_BYTES = Number(process.env.BIG_BYTES ?? 200_000);
const LEAK_BYTES = Number(process.env.LEAK_BYTES ?? 1024 * 1024);
const POOL_SIZE = Number(process.env.POOL_SIZE ?? 0); // 0: no shared pool
const HANG_UNKNOWN = process.env.HANG_UNKNOWN === '1';
const IGNORE_CANCEL = process.env.IGNORE_CANCEL === '1';
const SESSION_IDLE_MS = LEAK ? 0 : Number(process.env.SESSION_IDLE_MS ?? 300_000);
const SERVER_NAME =
  process.env.SERVER_NAME ??
  (LEAK ? 'ts-leaky' : REQUIRE_AUTH_URL ? 'ts-oauth' : POOL_SIZE > 0 ? 'ts-pooled' : IGNORE_CANCEL ? 'ts-ignore-cancel' : 'ts-healthy');
const REPLICA = process.env.HOSTNAME ?? 'local';
const TRACK_CALLS = process.env.TRACK_CALLS === '1';
const CALL_LOG = process.env.CALL_LOG ?? '/tmp/mcpload-calls.log';
const DEDUPE = process.env.DEDUPE ?? 'off'; // off | atomic | racy
const DEDUPE_RACE_MS = Number(process.env.DEDUPE_RACE_MS ?? 20);
const CALL_ID_KEY = 'io.mcpload/callId';

const BIG_TEXT = makeBigText(BIG_BYTES);

function makeBigText(n) {
  const line = 'mcpload big payload 0123456789 abcdefghijklmnopqrstuvwxyz ABCDEFGHIJKLMNOPQRSTUVWXYZ\n';
  return line.repeat(Math.ceil(n / line.length)).slice(0, n);
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
/** sleep that rejects as soon as `signal` aborts (the request was cancelled). */
const abortableSleep = (ms, signal) =>
  new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(new Error('cancelled'));
    const t = setTimeout(() => { signal?.removeEventListener('abort', onAbort); resolve(); }, ms);
    function onAbort() { clearTimeout(t); reject(new Error('cancelled')); }
    signal?.addEventListener('abort', onAbort, { once: true });
  });
const text = (t) => ({ content: [{ type: 'text', text: t }] });
const toolError = (t) => ({ isError: true, content: [{ type: 'text', text: t }] });

// POOL_SIZE > 0: one process-wide FIFO pool of slots that every tool call must hold while it runs.
const pool = { free: POOL_SIZE, waiting: [] };
async function pooled(fn) {
  if (POOL_SIZE <= 0) return fn();
  if (pool.free > 0) pool.free--;
  else await new Promise((resolve) => pool.waiting.push(resolve));
  try {
    return await fn();
  } finally {
    const next = pool.waiting.shift();
    if (next) next();
    else pool.free++;
  }
}

const knownTools = new Set(); // filled by registerTool

// Static resources: [name, uri, title, text].
const DEMO_DOCS = [
  ['readme', 'demo://docs/readme', 'Demo README', '# mcpload demo server\n\nA known-good MCP target for load tests. Tools: fast, slow, flaky, big, search.\n'],
  ['changelog', 'demo://docs/changelog', 'Demo changelog', '# Changelog\n\n## 0.1.0\n\n- Tools, resources and a prompt for mcpload.\n'],
];

// HANG_UNKNOWN=1: keep the request open without ever answering (the client's timeout ends it).
let hanging = 0;
function hang(req, why) {
  hanging++;
  if (hanging <= 5 || hanging % 100 === 0) console.warn(`HANG_UNKNOWN: not answering ${why} (${hanging} so far)`);
}

// ---- call tracking (TRACK_CALLS=1) ----
/** @type {Map<string, number>} callId -> times its handler started (this run of the process and earlier ones) */
const executions = new Map();
if (TRACK_CALLS && existsSync(CALL_LOG)) {
  for (const id of readFileSync(CALL_LOG, 'utf8').split('\n')) if (id) executions.set(id, (executions.get(id) ?? 0) + 1);
}

function recordCall(id) {
  const n = (executions.get(id) ?? 0) + 1;
  executions.set(id, n);
  appendFileSync(CALL_LOG, id + '\n'); // synchronous: the record survives an immediate exit
  callExecutions.inc();
  if (n > 1) duplicateExecutions.inc();
}

async function tracked(extra, fn) {
  const id = TRACK_CALLS ? extra?._meta?.[CALL_ID_KEY] : undefined;
  if (typeof id !== 'string' || !id) return fn();
  if (DEDUPE !== 'off' && executions.has(id)) {
    callsDeduplicated.inc();
    return text(`call ${id} already ran; skipped`);
  }
  if (DEDUPE === 'racy') await sleep(DEDUPE_RACE_MS); // deliberate bug: check, then act after an await
  recordCall(id);
  return fn();
}

// ---- cancellation accounting ----
const cancelledTotal = new client.Counter({
  name: 'mcp_cancelled_total', help: 'tools/call requests cancelled by the client while running.', labelNames: ['tool'],
});
const workAfterCancel = new client.Histogram({
  name: 'mcp_work_after_cancel_seconds',
  help: 'How long a cancelled tools/call kept running after its cancellation arrived.',
  labelNames: ['tool'],
  buckets: [0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 1.5, 2, 2.5, 5, 10, 30, 60],
});
const cancelledInflight = new client.Gauge({ name: 'mcp_cancelled_inflight', help: 'Cancelled tools/call handlers still running.' });

/**
 * Run one tool call, noting when it is cancelled (its abort signal fires, or onCancel calls back under
 * IGNORE_CANCEL) and how long it keeps running afterwards.
 */
async function cancellable(tool, extra, onCancel, fn) {
  let cancelledAt = 0;
  const cancelled = () => {
    if (cancelledAt) return;
    cancelledAt = performance.now();
    cancelledTotal.inc({ tool });
    cancelledInflight.inc();
  };
  const id = extra?.requestId;
  if (id !== undefined) onCancel.set(id, cancelled);
  extra?.signal?.addEventListener('abort', cancelled, { once: true });
  try {
    return await fn();
  } finally {
    if (id !== undefined) onCancel.delete(id);
    extra?.signal?.removeEventListener('abort', cancelled);
    if (cancelledAt) {
      workAfterCancel.observe({ tool }, (performance.now() - cancelledAt) / 1000);
      cancelledInflight.dec();
    }
  }
}

// ---- MCP server factory (one McpServer per session, as in the SDK examples) ----
function createMcpServer() {
  const server = new McpServer({ name: SERVER_NAME, version: '0.1.0' });
  const onCancel = new Map(); // requestId -> cancellable()'s callback
  if (IGNORE_CANCEL) {
    // Deliberate bug: replace the SDK's handler (which aborts the request's signal and drops its response)
    // with one that only records the cancel. The call keeps running and still answers.
    server.server.setNotificationHandler(CancelledNotificationSchema, (n) => onCancel.get(n.params.requestId)?.());
  }
  const register = server.registerTool.bind(server);
  // The handler's last argument is the request's `extra` (params._meta, abort signal, requestId).
  // cancellable() covers the whole call (pool wait included); tracked() records the call id once the
  // handler holds its pool slot (where a non-idempotent tool would act).
  server.registerTool = (name, meta, handler) => {
    knownTools.add(name);
    return register(name, meta, (...a) => {
      const extra = a[a.length - 1];
      return cancellable(name, extra, onCancel, () => pooled(() => tracked(extra, () => handler(...a))));
    });
  };

  server.registerTool('fast', { description: 'Returns immediately.', inputSchema: {} }, async () => text('ok'));

  server.registerTool(
    'slow',
    {
      description: 'Sleeps for `ms` milliseconds (default 300) before returning.',
      inputSchema: { ms: z.number().int().min(0).max(120_000).optional() },
    },
    async ({ ms }, extra) => {
      const d = ms ?? 300;
      // Stops when cancelled; IGNORE_CANCEL ignores the abort signal altogether (also when the session closes).
      await (IGNORE_CANCEL ? sleep(d) : abortableSleep(d, extra.signal));
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

  // Server-to-client requests, sent on this tools/call's own SSE stream (relatedRequestId). A client that did
  // not declare the capability gets isError (no request is sent), so runs without sampling/elicitation
  // configured just see a tool error. The demo tool mix never calls these two.
  server.registerTool(
    'sample_llm',
    {
      description: 'Asks the client to sample an LLM (sampling/createMessage) and returns its reply.',
      inputSchema: { prompt: z.string().optional(), maxTokens: z.number().int().min(1).max(4096).optional() },
    },
    async ({ prompt, maxTokens }, extra) => {
      if (!server.server.getClientCapabilities()?.sampling) return toolError('sample_llm: client does not support sampling');
      try {
        const r = await server.server.createMessage(
          { messages: [{ role: 'user', content: { type: 'text', text: prompt ?? 'Summarise the last search results.' } }], maxTokens: maxTokens ?? 100 },
          { relatedRequestId: extra.requestId },
        );
        return text(`sampled (${r.model}, ${r.stopReason ?? 'n/a'}): ${r.content?.type === 'text' ? r.content.text : r.content?.type}`);
      } catch (e) {
        return toolError(`sample_llm: ${e.message}`);
      }
    },
  );

  server.registerTool(
    'elicit_input',
    {
      description: 'Asks the user for input through the client (elicitation/create, form mode) and returns the answer.',
      inputSchema: { message: z.string().optional() },
    },
    async ({ message }, extra) => {
      if (!server.server.getClientCapabilities()?.elicitation) return toolError('elicit_input: client does not support elicitation');
      try {
        const r = await server.server.elicitInput(
          {
            message: message ?? 'Confirm the action?',
            requestedSchema: {
              type: 'object',
              properties: { confirm: { type: 'boolean', title: 'Confirm' }, note: { type: 'string', title: 'Note' } },
            },
          },
          { relatedRequestId: extra.requestId },
        );
        return text(`elicitation ${r.action}${r.content ? ': ' + JSON.stringify(r.content) : ''}`);
      } catch (e) {
        return toolError(`elicit_input: ${e.message}`);
      }
    },
  );

  // ---- resources and prompts (resources/list, resources/templates/list, resources/read, prompts/*) ----
  // Small fixed documents plus one template, so `resources/read` and `prompts/get` can be load-tested
  // (RESOURCE_READ_RATIO / PROMPT_GET_RATIO in scenarios/). They bypass the tool wrappers above (no pool,
  // call tracking or cancellation accounting).
  for (const [name, uri, title, body] of DEMO_DOCS) {
    server.registerResource(name, uri, { title, mimeType: 'text/markdown' }, async (u) => ({
      contents: [{ uri: u.href, mimeType: 'text/markdown', text: body }],
    }));
  }

  server.registerResource(
    'item',
    new ResourceTemplate('demo://items/{id}', { list: undefined }),
    { title: 'Demo item', description: 'A small JSON record for any id.', mimeType: 'application/json' },
    async (u, { id }) => ({
      contents: [{ uri: u.href, mimeType: 'application/json', text: JSON.stringify({ id, name: `Item ${id}`, price: (String(id).length * 7) % 100 }) }],
    }),
  );

  server.registerPrompt(
    'summarize',
    {
      title: 'Summarize a topic',
      description: 'Builds a one-message prompt asking for a short summary of `topic`.',
      argsSchema: { topic: z.string(), style: z.string().optional() },
    },
    async ({ topic, style }) => ({
      description: `Summary of ${topic}`,
      messages: [{ role: 'user', content: { type: 'text', text: `Summarize ${topic} in three sentences${style ? `, in a ${style} style` : ''}.` } }],
    }),
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
const callExecutions = new client.Counter({ name: 'mcp_tool_executions_total', help: 'Tool calls with a call id whose handler started (TRACK_CALLS=1).' });
const duplicateExecutions = new client.Counter({ name: 'mcp_tool_duplicate_executions_total', help: 'Executions of a call id that had already run (TRACK_CALLS=1).' });
const callsDeduplicated = new client.Counter({ name: 'mcp_tool_deduplicated_total', help: 'Calls skipped because their call id had already run (DEDUPE).' });

// ---- HTTP ----
const app = express();
app.disable('x-powered-by');
app.use((req, res, next) => { res.setHeader('X-Served-By', REPLICA); next(); });

app.get('/healthz', (_req, res) => res.json({ ok: true, name: SERVER_NAME, replica: REPLICA, sessions: sessions.size }));
app.get('/metrics', async (_req, res) => {
  res.setHeader('Content-Type', register.contentType);
  res.end(await register.metrics());
});
// Executed call ids (TRACK_CALLS=1), optionally only those starting with ?prefix= (one mcpload run).
app.get('/calls', (req, res) => {
  if (!TRACK_CALLS) return res.status(404).json({ error: 'call tracking is off (TRACK_CALLS=1)' });
  const prefix = String(req.query.prefix ?? '');
  const out = {};
  for (const [id, n] of executions) if (id.startsWith(prefix)) out[id] = n;
  res.json({ executions: out });
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
      if (!s) {
        sessionNotFound.inc();
        if (HANG_UNKNOWN) return hang(req, 'a request for an unknown session');
        return rpcError(res, 404, -32001, 'Session not found');
      }
      s.lastSeen = Date.now();
      const b = req.body;
      if (HANG_UNKNOWN && b && b.method === 'tools/call' && !knownTools.has(b.params?.name)) {
        return hang(req, `tools/call ${b.params?.name}`);
      }
      return await s.transport.handleRequest(req, res, req.body);
    }
    if (!isInitializeRequest(req.body)) {
      if (HANG_UNKNOWN) return hang(req, `${req.body?.method ?? 'a request'} without a session (protocol ${req.headers['mcp-protocol-version'] ?? 'unset'})`);
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
  console.log(`${SERVER_NAME} listening on :${PORT} (LEAK=${LEAK}, auth=${REQUIRE_AUTH_URL || 'off'}, idle=${SESSION_IDLE_MS}ms, ignoreCancel=${IGNORE_CANCEL}` +
    (TRACK_CALLS ? `, tracking calls in ${CALL_LOG} (${executions.size} known), dedupe=${DEDUPE}` : '') + ')');
});

for (const sig of ['SIGINT', 'SIGTERM']) process.on(sig, () => process.exit(0));
