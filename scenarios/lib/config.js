// requires k6 built with xk6-mcpload
//
// Shared configuration for the mcpload scenario library. Everything is read from
// environment variables (k6 -e NAME=value or the process environment).
//
//   MCP_URL        target endpoint                      (default http://localhost:3001/mcp)
//   MCP_PROTOCOL   auto | 2026-07-28 | 2025-06-18 | ...  (default auto)
//   MCP_TOKEN      static bearer token (auth type bearer)
//   OAUTH_TOKEN_URL, OAUTH_CLIENT_ID, OAUTH_CLIENT_SECRET   client-credentials OAuth (wins over MCP_TOKEN)
//   MCP_HEADERS    extra headers as JSON, e.g. {"X-Tenant":"load"}
//   MCP_TIMEOUT    per-request timeout                   (default 30s)
//   INCLUDE_PAYLOADS  1 to keep tool args/results        (default off)
//   TOOL_MIX       JSON weights, e.g. {"search":5,"fast":3,"slow":1}
//   TOOL_ARGS      JSON per-tool args, e.g. {"search":{"query":"invoices"}}
//   THINK_MS       mean think time between rounds (exponential distribution, capped at 5x)  (default 500)
//   PARALLEL       tools/call fired concurrently per round (default 3)
//   ROUNDS         rounds per session: "3" or a range "1-5" (default 1-5)
//   P95_MS, P99_MS, ERR_RATE     default per-tool budgets (800, 2000, 0.01)
//   TOOL_BUDGETS   per-tool overrides as JSON, e.g. {"slow":{"p95":1500,"p99":2500},"flaky":{"errRate":0.2}}
//   CONNECT_P95_MS budget for mcp_connect_duration p95 (default 1500)
//   CHECKS_MIN     minimum pass rate over all k6 checks (connect ok, tools/list, no transport error, ...) (default 0.99)
//   CONNECT_BACKOFF_MS  sleep after a failed connect() in agentSession so a dead target is not hot-looped (default 1000)

function env(name, def) {
  const v = __ENV[name];
  return v === undefined || v === '' ? def : v;
}

function envNum(name, def) {
  const v = env(name, undefined);
  if (v === undefined) return def;
  const n = Number(v);
  if (!Number.isFinite(n)) throw new Error(`${name} must be a number, got '${v}'`);
  return n;
}

function envJSON(name, def) {
  const v = env(name, undefined);
  if (v === undefined) return def;
  try {
    return JSON.parse(v);
  } catch (e) {
    throw new Error(`${name} must be valid JSON: ${e.message}`);
  }
}

function parseRange(s) {
  const m = /^\s*(\d+)\s*(?:-\s*(\d+))?\s*$/.exec(String(s));
  if (!m) throw new Error(`ROUNDS must be N or MIN-MAX, got '${s}'`);
  const min = Number(m[1]);
  const max = m[2] === undefined ? min : Number(m[2]);
  if (min < 1 || max < min) throw new Error(`ROUNDS range invalid: '${s}'`);
  return { min, max };
}

// Defaults match the demo servers (demo-servers/ts-server): fast, slow (300 ms), flaky (10% isError), big, search.
const DEFAULT_TOOL_MIX = { search: 5, fast: 3, slow: 1, big: 1, flaky: 1 };
// flaky fails 10% by design; 0.2 keeps a ~100-call run from tripping on sampling noise (0.15 is ~1.7 sd away).
const DEFAULT_TOOL_BUDGETS = { flaky: { errRate: 0.2 } };
const DEFAULT_TOOL_ARGS = {
  search: { query: 'invoices', limit: 5 },
  slow: {},
  big: {},
  flaky: {},
  fast: {},
};

function buildAuth() {
  const tokenUrl = env('OAUTH_TOKEN_URL', undefined);
  if (tokenUrl) {
    return {
      type: 'oauth',
      tokenUrl,
      clientId: env('OAUTH_CLIENT_ID', ''),
      clientSecret: env('OAUTH_CLIENT_SECRET', ''),
    };
  }
  const token = env('MCP_TOKEN', undefined);
  if (token) return { type: 'bearer', token };
  return undefined;
}

export const config = {
  url: env('MCP_URL', 'http://localhost:3001/mcp'),
  protocol: env('MCP_PROTOCOL', 'auto'),
  headers: envJSON('MCP_HEADERS', {}),
  auth: buildAuth(),
  timeout: env('MCP_TIMEOUT', '30s'),
  includePayloads: ['1', 'true', 'yes'].indexOf(String(env('INCLUDE_PAYLOADS', '')).toLowerCase()) >= 0,

  toolMix: envJSON('TOOL_MIX', DEFAULT_TOOL_MIX),
  toolMixExplicit: env('TOOL_MIX', undefined) !== undefined,
  toolArgs: Object.assign({}, DEFAULT_TOOL_ARGS, envJSON('TOOL_ARGS', {})),
  thinkMs: envNum('THINK_MS', 500),
  parallel: Math.max(1, Math.floor(envNum('PARALLEL', 3))),
  rounds: parseRange(env('ROUNDS', '1-5')),

  budgets: {
    p95: envNum('P95_MS', 800),
    p99: envNum('P99_MS', 2000),
    errRate: envNum('ERR_RATE', 0.01),
    connectP95: envNum('CONNECT_P95_MS', 1500),
    checksMin: envNum('CHECKS_MIN', 0.99),
  },
  connectBackoffMs: envNum('CONNECT_BACKOFF_MS', 1000),
  toolBudgets: envJSON('TOOL_BUDGETS', DEFAULT_TOOL_BUDGETS),
};

for (const [name, w] of Object.entries(config.toolMix)) {
  if (typeof w !== 'number' || !(w >= 0)) throw new Error(`TOOL_MIX weight for '${name}' must be a non-negative number`);
}

/** Options object for new mcp.Client(...). `overrides` are shallow-merged on top. */
export function clientOptions(overrides) {
  const o = {
    url: config.url,
    protocol: config.protocol,
    headers: config.headers,
    timeout: config.timeout,
    includePayloads: config.includePayloads,
  };
  if (config.auth) o.auth = config.auth;
  return Object.assign(o, overrides || {});
}

/** Budget for one tool: {p95, p99, errRate}. */
export function toolBudget(name) {
  return Object.assign(
    { p95: config.budgets.p95, p99: config.budgets.p99, errRate: config.budgets.errRate },
    (config.toolBudgets && config.toolBudgets[name]) || {},
  );
}

/**
 * k6 thresholds: per tool in TOOL_MIX, p95/p99 on mcp_req_duration and rate on mcp_tool_error_rate,
 * plus a connect-time budget and a minimum pass rate over all checks. The checks threshold is what fails a
 * run whose sessions never get going (every connect() fails: 401, refused, ...): the per-tool thresholds have
 * no samples then, and k6 passes a threshold without samples. `extra` is merged on top (same key replaces).
 */
export function buildThresholds(extra) {
  const t = {
    mcp_connect_duration: [`p(95)<${config.budgets.connectP95}`],
    checks: [`rate>=${config.budgets.checksMin}`],
  };
  for (const name of Object.keys(config.toolMix)) {
    if (!(config.toolMix[name] > 0)) continue;
    const b = toolBudget(name);
    t[`mcp_req_duration{tool:${name}}`] = [`p(95)<${b.p95}`, `p(99)<${b.p99}`];
    t[`mcp_tool_error_rate{tool:${name}}`] = [`rate<${b.errRate}`];
  }
  return Object.assign(t, extra || {});
}

/** Random integer in [min, max]. */
export function randInt(min, max) {
  return min + Math.floor(Math.random() * (max - min + 1));
}

/** Think time in seconds: exponential with mean THINK_MS, capped at 5x the mean. */
export function thinkSeconds() {
  if (config.thinkMs <= 0) return 0;
  const x = -Math.log(1 - Math.random()) * config.thinkMs;
  return Math.min(x, config.thinkMs * 5) / 1000;
}

/** Duration string like "90s" from seconds (k6 accepts s/m/h units). */
export function secs(n) {
  return `${Math.max(0, Math.round(n))}s`;
}

export { env, envNum, envJSON };
