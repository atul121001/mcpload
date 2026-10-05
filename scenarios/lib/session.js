// requires k6 built with xk6-mcpload
//
// One realistic agent session: connect -> tools/list -> N rounds of weighted-random
// parallel tools/call with think time -> close.
import mcp from 'k6/x/mcpload';
import exec from 'k6/execution';
import { check, sleep } from 'k6';
import { config, clientOptions, randInt, thinkSeconds } from './config.js';
import { planTools, pick } from './tools.js';
import { argsFromSchema } from './schema-args.js';
import { cancelOptions } from './cancel.js';
import { contentEnabled, entryLabel, parallelItem, pickContent, planContent, stepKind } from './content.js';

export { argsFromSchema, pick };

/** Create a client. Call this in the init context (top level of a scenario) so each VU gets one. */
export function makeClient(overrides) {
  return new mcp.Client(clientOptions(overrides));
}

let loggedErrors = 0;
function logError(msg) {
  // Keep logs readable under load: first 20 per VU only.
  if (loggedErrors++ < 20) console.warn(`[vu ${__VU}] ${msg}`);
}

let warnedUnknown = false;
let warnedNoContent = false;

/**
 * List what this session can read and get (RESOURCE_READ_RATIO / PROMPT_GET_RATIO > 0): resources/list and
 * resources/templates/list when reads are on, prompts/list when gets are on. A list the server rejects (e.g. no
 * resources capability) counts as empty. Warns once per VU when a kind is asked for but nothing is listed; such
 * steps then stay tool calls. Returns a lib/content.js#planContent() result.
 */
export function listContent(s) {
  const cc = config.content;
  const tryList = (fn, what) => {
    try {
      return fn();
    } catch (e) {
      if (!warnedNoContent) logError(`${what} failed: ${e}`);
      return undefined;
    }
  };
  const resources = cc.resourceRatio > 0 ? tryList(() => s.listResources(), 'resources/list') : undefined;
  const templates = cc.resourceRatio > 0 ? tryList(() => s.listResourceTemplates(), 'resources/templates/list') : undefined;
  const prompts = cc.promptRatio > 0 ? tryList(() => s.listPrompts(), 'prompts/list') : undefined;
  const plan = planContent(resources, templates, prompts);
  if (!warnedNoContent) {
    const missing = [];
    if (cc.resourceRatio > 0 && !plan.resources.length) missing.push('RESOURCE_READ_RATIO is set but the server lists no resources or resource templates');
    if (cc.promptRatio > 0 && !plan.prompts.length) missing.push('PROMPT_GET_RATIO is set but the server lists no prompts');
    if (missing.length) {
      warnedNoContent = true;
      console.warn(`[vu ${__VU}] ${missing.join('; ')}: those steps stay tool calls`);
    }
  }
  return plan;
}

/**
 * Build the weighted tool table for this session from what the server listed (see lib/tools.js#planTools):
 * explicit TOOL_MIX -> its listed tools; unset -> demo mix on a demo server (all five demo tools listed),
 * otherwise uniform over every listed tool. Warns once per VU about TOOL_MIX names the server doesn't list.
 */
export function toolTable(listed) {
  const tt = planTools(listed, config);
  if (tt.unknown.length && tt.table.length && !warnedUnknown) {
    warnedUnknown = true;
    console.warn(
      `[vu ${__VU}] TOOL_MIX names not listed by the server (skipped): ${tt.unknown.join(', ')}; server lists: ${tt.listedNames.join(', ')}`,
    );
  }
  return tt;
}

/**
 * Record the 'tool mix matched' check. When nothing matched (e.g. a TOOL_MIX typo) warn with the unknown and
 * listed names and back off CONNECT_BACKOFF_MS so the VU does not hot-loop. Returns true if there are tools to call.
 */
export function checkToolTable(tt, log) {
  const ok = tt.table.length > 0;
  check(null, { 'tool mix matched': () => ok });
  if (!ok) {
    (log || logError)(
      tt.unknown.length
        ? `TOOL_MIX matches no tool the server lists: unknown ${tt.unknown.join(', ')}; server lists: ${tt.listedNames.join(', ') || '(none)'}`
        : `no callable tools (server lists: ${tt.listedNames.join(', ') || '(none)'})`,
    );
    if (config.connectBackoffMs > 0) sleep(config.connectBackoffMs / 1000);
  }
  return ok;
}

function runGroup(s, batch, idx, results, tagDefault) {
  if (!idx.length) return;
  const tags = exec.vu.metrics.tags;
  if (tagDefault) tags.budget = 'default';
  try {
    // Per-call options: meta (params._meta) and, with CANCEL_RATE, { cancelAfterMs } (lib/cancel.js; tools/call only).
    const co = idx.map((i) => {
      const c = batch[i].kind ? undefined : cancelOptions(batch[i].name, config.cancel);
      return batch[i].meta ? Object.assign({ meta: batch[i].meta }, c || {}) : c;
    });
    let rs;
    if (idx.length === 1) {
      const e = batch[idx[0]];
      if (e.kind === 'resource') rs = [s.readResource(e.uri, co[0])];
      else if (e.kind === 'prompt') rs = [s.getPrompt(e.name, e.args, co[0])];
      else rs = [s.callTool(e.name, e.args, co[0])];
    } else rs = s.callParallel(idx.map((i, j) => parallelItem(batch[i], co[j])));
    for (let j = 0; j < idx.length; j++) results[idx[j]] = rs[j];
  } finally {
    if (tagDefault) delete tags.budget;
  }
}

/**
 * Call a batch of [{name, args, ownBudget, meta?}] (entries from toolTable()/pick(); meta is sent as params._meta).
 * Entries may also be resource reads / prompt gets from lib/content.js#pickContent ({kind: 'resource', uri} or
 * {kind: 'prompt', name, args}); they have no threshold of their own, so they run in the budget=default group.
 * Calls to tools without their own
 * threshold run as a separate callParallel group with the VU tag budget=default, so the catch-all thresholds
 * mcp_req_duration{budget:default} / mcp_tool_error_rate{budget:default} cover them (and only them).
 * Returns results in input order.
 */
export function callTools(s, batch) {
  const own = [];
  const dflt = [];
  for (let i = 0; i < batch.length; i++) (batch[i].ownBudget ? own : dflt).push(i);
  const results = new Array(batch.length);
  runGroup(s, batch, own, results, false);
  runGroup(s, batch, dflt, results, true);
  return results;
}

/**
 * Run one agent session.
 * opts: { rounds?: number, parallel?: number, think?: boolean (default true), listTools?: boolean (default true),
 *         onSession?: (s) => void  called after connect (e.g. to add pings),
 *         tableFilter?: (tt) => tt  adjusts the session's tool table before any call (e.g. drop tools),
 *         beforeRound?: (r) => void  called before each round of calls (e.g. to retag the VU) }
 * With RESOURCE_READ_RATIO / PROMPT_GET_RATIO set, the session also lists resources/prompts once after tools/list,
 * and that share of the calls in each round are resources/read / prompts/get instead (lib/content.js).
 * Returns { ok, protocol, sessionId, calls, toolErrors, cancelled, resourceReads, promptGets }
 * (`calls` and `toolErrors` include the reads and gets).
 */
export function agentSession(client, opts) {
  const o = opts || {};
  const rounds = o.rounds !== undefined ? o.rounds : randInt(config.rounds.min, config.rounds.max);
  const parallel = o.parallel !== undefined ? o.parallel : config.parallel;
  const out = { ok: false, protocol: undefined, sessionId: undefined, calls: 0, toolErrors: 0, cancelled: 0, resourceReads: 0, promptGets: 0 };

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

    let tt = toolTable(listed);
    if (o.tableFilter) tt = o.tableFilter(tt);
    if (!checkToolTable(tt)) return out;
    if (o.onSession) o.onSession(s);
    const content = contentEnabled(config.content) ? listContent(s) : null;

    for (let r = 0; r < rounds; r++) {
      if (o.beforeRound) o.beforeRound(r);
      const batch = [];
      for (let i = 0; i < parallel; i++) {
        const kind = content ? stepKind(config.content, content) : 'tool';
        if (kind === 'tool') batch.push(pick(tt));
        else {
          batch.push(pickContent(content, kind));
          if (kind === 'resource') out.resourceReads++;
          else out.promptGets++;
        }
      }
      const results = callTools(s, batch);
      out.calls += results.length;
      for (let i = 0; i < results.length; i++) {
        const res = results[i];
        // Calls cancelled on purpose (CANCEL_RATE) are counted apart and are not errors.
        if (res.cancelled) {
          out.cancelled++;
          continue;
        }
        // isError results are expected (e.g. `flaky`): counted in mcp_tool_error_rate, not logged.
        if (res.error && res.error.type !== 'tool_iserror') logError(`${entryLabel(batch[i])}: ${res.error.type}: ${res.error.message}`);
        if (res.error || res.isError) out.toolErrors++;
      }
      check(results, {
        'tools/call no transport error': (rs) => rs.every((x) => !x.error || x.error.type === 'tool_iserror' || x.cancelled),
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
