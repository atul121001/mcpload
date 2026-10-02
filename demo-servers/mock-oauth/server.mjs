// Demo only — not a real authorization server. Credentials are public.
//
// mcpload demo: minimal OAuth 2.0 token server (no dependencies).
//   POST /token       grant_type=client_credentials (form-encoded; client creds via Basic auth or body)
//                     -> {access_token, token_type:"Bearer", expires_in}
//   POST /introspect  token=... (RFC 7662) -> {active:true, client_id, exp, ...} | {active:false}
//   GET  /healthz, GET /metrics (plain Prometheus text)
// Env: PORT (3000), TOKEN_TTL_SECONDS (30), CLIENTS ("id:secret,..." ; empty = accept any client).
import http from 'node:http';
import { randomBytes } from 'node:crypto';

const PORT = Number(process.env.PORT ?? 3000);
const TTL = Number(process.env.TOKEN_TTL_SECONDS ?? 30);
const CLIENTS = new Map((process.env.CLIENTS ?? 'mcpload:secret').split(',').filter(Boolean).map((p) => p.split(':')));
const tokens = new Map(); // token -> {client_id, scope, exp}
const stats = { issued: 0, introspect_active: 0, introspect_inactive: 0, token_rejected: 0 };

setInterval(() => { const now = Date.now() / 1000; for (const [t, v] of tokens) if (v.exp < now) tokens.delete(t); }, 10_000).unref();

function send(res, status, obj, headers = {}) {
  res.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store', ...headers });
  res.end(JSON.stringify(obj));
}

function readForm(req) {
  return new Promise((resolve, reject) => {
    let s = '';
    req.on('data', (c) => { s += c; if (s.length > 65536) req.destroy(); });
    req.on('end', () => {
      const ct = req.headers['content-type'] ?? '';
      try { resolve(ct.includes('application/json') ? JSON.parse(s || '{}') : Object.fromEntries(new URLSearchParams(s))); } catch (e) { reject(e); }
    });
    req.on('error', reject);
  });
}

http.createServer(async (req, res) => {
  const url = new URL(req.url, 'http://x');
  try {
    if (req.method === 'GET' && url.pathname === '/healthz') return send(res, 200, { ok: true, live_tokens: tokens.size });
    if (req.method === 'GET' && url.pathname === '/metrics') {
      res.writeHead(200, { 'Content-Type': 'text/plain; version=0.0.4' });
      return res.end(
        `# TYPE oauth_tokens_issued_total counter\noauth_tokens_issued_total ${stats.issued}\n` +
        `# TYPE oauth_token_requests_rejected_total counter\noauth_token_requests_rejected_total ${stats.token_rejected}\n` +
        `# TYPE oauth_introspect_total counter\noauth_introspect_total{active="true"} ${stats.introspect_active}\noauth_introspect_total{active="false"} ${stats.introspect_inactive}\n` +
        `# TYPE oauth_live_tokens gauge\noauth_live_tokens ${tokens.size}\n` +
        `# TYPE process_resident_memory_bytes gauge\nprocess_resident_memory_bytes ${process.memoryUsage().rss}\n`);
    }
    if (req.method === 'POST' && url.pathname === '/token') {
      const body = await readForm(req);
      let id = body.client_id, secret = body.client_secret;
      const basic = /^Basic\s+(.+)$/i.exec(req.headers.authorization ?? '');
      if (basic) [id, secret] = Buffer.from(basic[1], 'base64').toString().split(':');
      if (body.grant_type !== 'client_credentials') { stats.token_rejected++; return send(res, 400, { error: 'unsupported_grant_type' }); }
      if (CLIENTS.size && CLIENTS.get(id) !== secret) { stats.token_rejected++; return send(res, 401, { error: 'invalid_client' }, { 'WWW-Authenticate': 'Basic realm="mock-oauth"' }); }
      const access_token = randomBytes(24).toString('base64url');
      const exp = Math.floor(Date.now() / 1000) + TTL;
      tokens.set(access_token, { client_id: id ?? 'anonymous', scope: body.scope ?? 'mcp', exp });
      stats.issued++;
      return send(res, 200, { access_token, token_type: 'Bearer', expires_in: TTL, scope: body.scope ?? 'mcp' });
    }
    if (req.method === 'POST' && url.pathname === '/introspect') {
      const { token } = await readForm(req);
      const t = token && tokens.get(token);
      if (!t || t.exp <= Date.now() / 1000) { stats.introspect_inactive++; return send(res, 200, { active: false }); }
      stats.introspect_active++;
      return send(res, 200, { active: true, client_id: t.client_id, scope: t.scope, exp: t.exp, token_type: 'Bearer' });
    }
    send(res, 404, { error: 'not_found' });
  } catch (e) {
    send(res, 400, { error: 'invalid_request', error_description: String(e.message ?? e) });
  }
}).listen(PORT, '0.0.0.0', () => console.log(`mock-oauth listening on :${PORT} (ttl=${TTL}s, clients=${[...CLIENTS.keys()].join(',') || 'any'})`));

for (const sig of ['SIGINT', 'SIGTERM']) process.on(sig, () => process.exit(0));
