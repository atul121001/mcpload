// Pure helpers (no k6 imports) for scenarios/version-skew.js: classify a failed request as a fast typed error
// or a hang, and parse the tools/list cache TTL. Unit-tested by skew.test.mjs.

// JSON-RPC codes a mismatched build answers with.
export const CODE_UNSUPPORTED_PROTOCOL = -32022;
export const CODE_METHOD_NOT_FOUND = -32601;
export const CODE_INVALID_PARAMS = -32602;

const UNKNOWN_TOOL = /unknown tool|tool\b.*\bnot found|no such tool|tool\b.*\bnot (?:registered|available)/i;

/** True when an error says the tool doesn't exist on the replica that answered (an isError result or a JSON-RPC error). */
export function isUnknownTool(err) {
  if (!err) return false;
  const typed = err.type === 'tool_iserror' || err.type === 'jsonrpc' || err.code === CODE_INVALID_PARAMS;
  return typed && UNKNOWN_TOOL.test(String(err.message || ''));
}

/**
 * Why a request failed, used as the `reason` metric tag:
 * timeout, unsupported_protocol (-32022), method_not_found (-32601), unknown_tool, session_not_found (404 for a
 * session id), header_mismatch, auth, http_<status> (any other HTTP 4xx/5xx), jsonrpc_error (a JSON-RPC error
 * on HTTP 2xx), transport (no HTTP response, e.g. connection reset).
 */
export function failureReason(err) {
  if (!err) return '';
  if (err.type === 'timeout') return 'timeout';
  if (err.code === CODE_UNSUPPORTED_PROTOCOL) return 'unsupported_protocol';
  if (err.code === CODE_METHOD_NOT_FOUND) return 'method_not_found';
  if (isUnknownTool(err)) return 'unknown_tool';
  if (err.type === 'session_not_found' || err.type === 'header_mismatch' || err.type === 'auth') return err.type;
  if (err.status >= 400) return `http_${err.status}`;
  if (err.code) return 'jsonrpc_error';
  return 'transport';
}

/**
 * Classify one request. `err` is the error of a tools/call result (`res.error`) or the error thrown by
 * connect()/listTools()/close(), or null/undefined on success; `ms` is how long the request took.
 * Returns null when the request did not fail because of the deployment: success, or an ordinary tool-level
 * isError result (e.g. the demo `flaky` tool). Otherwise {kind, reason, typed}:
 *   hang  error type `timeout`, or the failure took at least hangMs
 *   fast  a typed error (HTTP 4xx/5xx, a JSON-RPC error code, or an "unknown tool" result) in under failFastMs
 *   slow  everything else: a typed error between failFastMs and hangMs, or an untyped one (no HTTP status)
 */
export function classifyFailure(err, ms, opts) {
  if (!err) return null;
  if (err.type === 'tool_iserror' && !isUnknownTool(err)) return null;
  const o = opts || {};
  const failFastMs = o.failFastMs === undefined ? 2000 : o.failFastMs;
  const hangMs = o.hangMs === undefined ? 10000 : o.hangMs;
  const reason = failureReason(err);
  const typed = reason !== 'timeout' && reason !== 'transport';
  let kind = 'slow';
  if (reason === 'timeout' || ms >= hangMs) kind = 'hang';
  else if (typed && ms < failFastMs) kind = 'fast';
  return { kind, reason, typed };
}

const UNIT_MS = { ms: 1, s: 1000, m: 60000, h: 3600000 };

/** TOOLS_CACHE_TTL: '' / '0' / 'off' -> 0 (re-list every session); '5m', '30s', '1h', '500ms' or plain milliseconds. */
export function parseTtlMs(s) {
  const v = String(s === undefined || s === null ? '' : s).trim().toLowerCase();
  if (v === '' || v === '0' || v === 'off' || v === 'false' || v === 'no') return 0;
  if (/^\d+(\.\d+)?$/.test(v)) return Number(v);
  const m = /^(\d+(?:\.\d+)?)\s*(ms|s|m|h)$/.exec(v);
  if (!m) throw new Error(`TOOLS_CACHE_TTL must be a duration like 5m, 30s or 500ms (or 0 to turn the cache off), got '${s}'`);
  return Number(m[1]) * UNIT_MS[m[2]];
}

/**
 * A tools/list result kept across sessions for ttlMs, as a client that honours a list's cache TTL does.
 * `get(now)` returns the cached list while it is fresh, else null.
 */
export class ToolListCache {
  constructor(ttlMs) {
    this.ttlMs = ttlMs;
    this.tools = null;
    this.at = 0;
    this.from = '';
  }

  get(now) {
    if (!(this.ttlMs > 0) || !this.tools || now - this.at >= this.ttlMs) return null;
    return this.tools;
  }

  put(tools, now, from) {
    if (!(this.ttlMs > 0)) return;
    this.tools = tools;
    this.at = now;
    this.from = from || '';
  }
}
