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
//   TOOL_MIX       JSON weights, e.g. {"search":5,"fast":3,"slow":1}. Unset: the demo mix if the server lists all
//                  five demo tools (fast, slow, big, flaky, search), otherwise uniform weight over every listed tool.
//   TOOL_ARGS      JSON per-tool args, e.g. {"search":{"query":"invoices"}}. Other tools get placeholder args
//                  derived from their inputSchema (or the demo args on a demo server).
//   THINK_MS       mean think time between rounds (exponential distribution, capped at 5x)  (default 500)
//   PARALLEL       tools/call fired concurrently per round (default 3)
//   ROUNDS         rounds per session: "3" or a range "1-5" (default 1-5)
//   P95_MS, P99_MS, ERR_RATE     default per-tool budgets (800, 2000, 0.01); also the catch-all budget
//                  (tag budget:default) applied to every called tool that has no threshold of its own
//   TOOL_BUDGETS   per-tool overrides as JSON, e.g. {"slow":{"p95":1500,"p99":2500}}; merged over the built-in
//                  {"flaky":{"errRate":0.2}} (which only covers `flaky` on a demo server)
//   CONNECT_P95_MS budget for mcp_connect_duration p95 (default 1500)
//   CHECKS_MIN     minimum pass rate over all k6 checks (connect ok, tools/list, tool mix matched, no transport
//                  error, ...) (default 0.99)
//   CONNECT_BACKOFF_MS  sleep after a failed connect() (or a TOOL_MIX that matches no listed tool) so a dead or
//                  misconfigured target is not hot-looped (default 1000)
//   SAMPLING, ELICITATION, ROOTS  answer the server's sampling/createMessage, elicitation/create or roots/list
//                  requests (stateful servers only) and declare the capability: "1" for the default mock answer,
//                  or the client option as JSON, e.g. {"delayMs":200} or {"action":"decline"} (default off)
import { thresholdToolNames } from './tools.js';

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

function envObject(name) {
  const v = envJSON(name, undefined);
  if (v === undefined) return undefined;
  if (v === null || typeof v !== 'object' || Array.isArray(v)) throw new Error(`${name} must be a JSON object`);
  return v;
}

function parseRange(s) {
  const m = /^\s*(\d+)\s*(?:-\s*(\d+))?\s*$/.exec(String(s));
  if (!m) throw new Error(`ROUNDS must be N or MIN-MAX, got '${s}'`);
  const min = Number(m[1]);
  const max = m[2] === undefined ? min : Number(m[2]);
  if (min < 1 || max < min) throw new Error(`ROUNDS range invalid: '${s}'`);
  return { min, max };
}

// flaky (demo servers) fails 10% by design; 0.2 keeps a ~100-call run from tripping on sampling noise.
const DEFAULT_TOOL_BUDGETS = { flaky: { errRate: 0.2 } };

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

/** SAMPLING / ELICITATION / ROOTS: unset or "0" -> off, "1"/"true" -> true (default answer), else a JSON object. */
function envResponder(name) {
  const v = env(name, undefined);
  if (v === undefined || ['0', 'false', 'no'].indexOf(String(v).toLowerCase()) >= 0) return undefined;
  if (['1', 'true', 'yes'].indexOf(String(v).toLowerCase()) >= 0) return true;
  return envObject(name);
}

const userBudgets = envObject('TOOL_BUDGETS') || {};

export const config = {
  url: env('MCP_URL', 'http://localhost:3001/mcp'),
  protocol: env('MCP_PROTOCOL', 'auto'),
  headers: envJSON('MCP_HEADERS', {}),
  auth: buildAuth(),
  timeout: env('MCP_TIMEOUT', '30s'),
  includePayloads: ['1', 'true', 'yes'].indexOf(String(env('INCLUDE_PAYLOADS', '')).toLowerCase()) >= 0,
  sampling: envResponder('SAMPLING'),
  elicitation: envResponder('ELICITATION'),
  roots: envResponder('ROOTS'),

  // Explicit TOOL_MIX, or null: demo mix on a demo server, else uniform over listed tools (lib/tools.js).
  toolMix: envObject('TOOL_MIX') || null,
  // Explicit TOOL_ARGS only; lib/tools.js adds the demo args on a demo server.
  toolArgs: envObject('TOOL_ARGS') || {},
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
  // Explicit TOOL_BUDGETS: a tool named here always has (and is covered by) its own threshold.
  userBudgets,
  toolBudgets: Object.assign({}, DEFAULT_TOOL_BUDGETS, userBudgets),
};
config.toolMixExplicit = config.toolMix !== null;

for (const [name, w] of Object.entries(config.toolMix || {})) {
  if (typeof w !== 'number' || !(w >= 0)) throw new Error(`TOOL_MIX weight for '${name}' must be a non-negative number`);
}
if (config.toolMix && !Object.values(config.toolMix).some((w) => w > 0)) {
  throw new Error('TOOL_MIX needs at least one tool with weight > 0');
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
  for (const k of ['sampling', 'elicitation', 'roots']) if (config[k] !== undefined) o[k] = config[k];
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
 * k6 thresholds:
 *   - per tool: p95/p99 on mcp_req_duration{tool:<name>} and rate on mcp_tool_error_rate{tool:<name>} for the
 *     explicit TOOL_MIX names (or the demo tool names when TOOL_MIX is unset) and every TOOL_BUDGETS name;
 *   - catch-all: the same P95_MS/P99_MS/ERR_RATE budgets on calls tagged budget:default. lib/session.js tags
 *     every call to a tool that is not covered by its own threshold (e.g. a real server's tool names), so no
 *     called tool is ever unbudgeted;
 *   - a connect-time budget and a minimum pass rate over all checks. The checks threshold is what fails a run
 *     whose sessions never get going (every connect() fails: 401, refused, ...; or TOOL_MIX matches no listed
 *     tool): the tool thresholds have no samples then, and k6 passes a threshold without samples.
 * `extra` is merged on top (same key replaces).
 */
export function buildThresholds(extra) {
  const b = config.budgets;
  const t = {
    mcp_connect_duration: [`p(95)<${b.connectP95}`],
    checks: [`rate>=${b.checksMin}`],
    'mcp_req_duration{budget:default}': [`p(95)<${b.p95}`, `p(99)<${b.p99}`],
    'mcp_tool_error_rate{budget:default}': [`rate<${b.errRate}`],
  };
  for (const name of thresholdToolNames(config)) {
    const tb = toolBudget(name);
    t[`mcp_req_duration{tool:${name}}`] = [`p(95)<${tb.p95}`, `p(99)<${tb.p99}`];
    t[`mcp_tool_error_rate{tool:${name}}`] = [`rate<${tb.errRate}`];
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

const UNIT_SECONDS = { ms: 0.001, s: 1, m: 60, h: 3600 };

/** Seconds in a k6-style duration like "1s", "1m", "500ms", "1h". */
export function durationSeconds(d) {
  const m = /^\s*(\d+(?:\.\d+)?)\s*(ms|s|m|h)\s*$/.exec(String(d));
  if (!m) throw new Error(`expected a duration like 1s, 1m or 1h, got '${d}'`);
  return Number(m[1]) * UNIT_SECONDS[m[2]];
}

/**
 * Arrival-rate executors need an integer `rate` per `timeUnit`. Turn `rate` per `timeUnit` (both may be
 * fractional, e.g. RATE=0.05 per 1s) into the smallest of 1s / 1m / 1h for which the rate is an integer
 * (0.05/s -> 3 per 1m). Rates finer than 1/h are rounded per hour (minimum 1).
 * Returns { rate, timeUnit, perSecond }.
 */
export function arrivalRate(rate, timeUnit) {
  if (!(rate > 0)) throw new Error(`RATE must be > 0, got '${rate}'`);
  const perSecond = rate / durationSeconds(timeUnit || '1s');
  if (Number.isInteger(rate) && /^\s*1\s*(s|m|h)\s*$/.test(String(timeUnit || '1s'))) {
    return { rate, timeUnit: String(timeUnit || '1s').trim(), perSecond };
  }
  for (const [unit, sec] of [['1s', 1], ['1m', 60], ['1h', 3600]]) {
    const r = perSecond * sec;
    if (Math.abs(r - Math.round(r)) < 1e-9 && Math.round(r) >= 1) return { rate: Math.round(r), timeUnit: unit, perSecond };
  }
  return { rate: Math.max(1, Math.round(perSecond * 3600)), timeUnit: '1h', perSecond };
}

export { env, envNum, envJSON };
