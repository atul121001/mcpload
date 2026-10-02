// requires k6 built with xk6-mcpload
//
// One realistic agent session: connect -> tools/list -> N rounds of weighted-random
// parallel tools/call with think time -> close.
import mcp from 'k6/x/mcpload';
import { check, sleep } from 'k6';
import { config, clientOptions, randInt, thinkSeconds } from './config.js';

/** Create a client. Call this in the init context (top level of a scenario) so each VU gets one. */
export function makeClient(overrides) {
  return new mcp.Client(clientOptions(overrides));
}

/** Placeholder args for a tool's required properties, derived from its JSON Schema. */
export function argsFromSchema(inputSchema) {
  const args = {};
  if (!inputSchema || typeof inputSchema !== 'object') return args;
  const props = inputSchema.properties || {};
  for (const key of inputSchema.required || []) {
    const p = props[key] || {};
    if (p.default !== undefined) args[key] = p.default;
    else if (Array.isArray(p.enum) && p.enum.length) args[key] = p.enum[0];
    else if (p.type === 'string') args[key] = 'load-test';
    else if (p.type === 'integer' || p.type === 'number') args[key] = typeof p.minimum === 'number' ? p.minimum : 1;
    else if (p.type === 'boolean') args[key] = false;
    else if (p.type === 'array') args[key] = [];
    else if (p.type === 'object') args[key] = {};
    else args[key] = 'load-test';
  }
  return args;
}

/**
 * Build the weighted tool table for this session from what the server listed.
 * If TOOL_MIX was set explicitly, only listed tools with weight > 0 are used.
 * Otherwise the default mix is used where it matches, falling back to uniform over listed tools.
 */
export function toolTable(listed) {
  const byName = {};
  for (const t of listed) byName[t.name] = t;
  const table = [];
  let total = 0;
  for (const name of Object.keys(config.toolMix)) {
    const w = config.toolMix[name];
    if (!(w > 0) || !byName[name]) continue;
    total += w;
    table.push({ name, cum: total, args: toolArgs(name, byName[name]) });
  }
  if (table.length === 0 && !config.toolMixExplicit) {
    for (const t of listed) {
      total += 1;
      table.push({ name: t.name, cum: total, args: toolArgs(t.name, t) });
    }
  }
  return { table, total };
}

function toolArgs(name, tool) {
  if (config.toolArgs[name] !== undefined) return config.toolArgs[name];
  return argsFromSchema(tool && tool.inputSchema);
}

/** Weighted random pick from a toolTable(). */
export function pick(tt) {
  const r = Math.random() * tt.total;
  for (const e of tt.table) if (r < e.cum) return e;
  return tt.table[tt.table.length - 1];
}

let loggedErrors = 0;
function logError(msg) {
  // Keep logs readable under load: first 20 per VU only.
  if (loggedErrors++ < 20) console.warn(`[vu ${__VU}] ${msg}`);
}

/**
 * Run one agent session.
 * opts: { rounds?: number, parallel?: number, think?: boolean (default true), listTools?: boolean (default true),
 *         onSession?: (s) => void  called after connect (e.g. to add pings) }
 * Returns { ok, protocol, sessionId, calls, toolErrors }.
 */
export function agentSession(client, opts) {
  const o = opts || {};
  const rounds = o.rounds !== undefined ? o.rounds : randInt(config.rounds.min, config.rounds.max);
  const parallel = o.parallel !== undefined ? o.parallel : config.parallel;
  const out = { ok: false, protocol: undefined, sessionId: undefined, calls: 0, toolErrors: 0 };

  let s;
  try {
    s = client.connect();
  } catch (e) {
    logError(`connect failed: ${e}`);
    check(null, { 'connect ok': () => false });
    // Back off so a dead/unauthorised target is not hammered in a hot loop (thousands of connects/s).
    if (config.connectBackoffMs > 0) sleep(config.connectBackoffMs / 1000);
    return out;
  }
  check(s, { 'connect ok': () => true });
  out.protocol = s.protocol;
  out.sessionId = s.sessionId;

  try {
    const listed = s.listTools();
    const listOk = check(listed, { 'tools/list returned tools': (l) => Array.isArray(l) && l.length > 0 });
    if (!listOk) return out;

    const tt = toolTable(listed);
    if (tt.table.length === 0) {
      logError(`no tools from TOOL_MIX are offered by the server (listed: ${listed.map((t) => t.name).join(', ')})`);
      return out;
    }
    if (o.onSession) o.onSession(s);

    for (let r = 0; r < rounds; r++) {
      const batch = [];
      for (let i = 0; i < parallel; i++) {
        const e = pick(tt);
        batch.push({ name: e.name, args: e.args });
      }
      const results = batch.length === 1 ? [s.callTool(batch[0].name, batch[0].args)] : s.callParallel(batch);
      out.calls += results.length;
      for (let i = 0; i < results.length; i++) {
        const res = results[i];
        // isError results are expected (e.g. `flaky`): counted in mcp_tool_error_rate, not logged.
        if (res.error && res.error.type !== 'tool_iserror') logError(`${batch[i].name}: ${res.error.type}: ${res.error.message}`);
        if (res.error || res.isError) out.toolErrors++;
      }
      check(results, {
        'tools/call no transport error': (rs) => rs.every((x) => !x.error || x.error.type === 'tool_iserror'),
      });
      if (o.think !== false && r < rounds - 1) sleep(thinkSeconds());
    }
    out.ok = true;
    return out;
  } catch (e) {
    logError(`session error: ${e}`);
    return out;
  } finally {
    try {
      s.close();
    } catch (e) {
      logError(`close failed: ${e}`);
    }
  }
}
