// Pure helpers (no k6 imports) deciding which tools a session calls, with which args, and whether each
// call is covered by its own per-tool threshold or by the catch-all `budget:default` one.
// Unit-tested by schema-args.test.mjs.
import { argsFromSchema } from './schema-args.js';

/** The demo servers (demo-servers/) all list exactly these tools. */
export const DEMO_TOOLS = ['fast', 'slow', 'big', 'flaky', 'search'];
export const DEMO_TOOL_MIX = { search: 5, fast: 3, slow: 1, big: 1, flaky: 1 };
export const DEMO_TOOL_ARGS = { search: { query: 'invoices', limit: 5 }, slow: {}, big: {}, flaky: {}, fast: {} };

/** True when the server lists all five demo tools; only then are the demo mix and demo args used. */
export function isDemoServer(listedNames) {
  return DEMO_TOOLS.every((n) => listedNames.indexOf(n) >= 0);
}

/**
 * Names that get their own `{tool:<name>}` thresholds. Thresholds are fixed before any server is contacted,
 * so this is: the explicit TOOL_MIX names (weight > 0), or the demo names when TOOL_MIX is unset,
 * plus every TOOL_BUDGETS name (built-in flaky default included).
 */
export function thresholdToolNames(cfg) {
  const names = [];
  const add = (n) => {
    if (names.indexOf(n) < 0) names.push(n);
  };
  if (cfg.toolMix) {
    for (const n of Object.keys(cfg.toolMix)) if (cfg.toolMix[n] > 0) add(n);
  } else DEMO_TOOLS.forEach(add);
  for (const n of Object.keys(cfg.toolBudgets || {})) add(n);
  return names;
}

/**
 * Plan a session's tool table from what the server listed.
 * cfg: { toolMix: object|null (explicit TOOL_MIX), toolArgs: object (explicit TOOL_ARGS), userBudgets: object (explicit TOOL_BUDGETS) }
 * Returns { table: [{name, cum, args, ownBudget}], total, demo, unknown, listedNames }.
 *   - TOOL_MIX set: only its listed tools with weight > 0; `unknown` = its names the server doesn't list.
 *   - TOOL_MIX unset and the server lists all demo tools: the demo mix.
 *   - otherwise: uniform weight over every listed tool.
 * Args: TOOL_ARGS[name], else demo args (demo servers only), else placeholders from inputSchema.
 * ownBudget: the call is covered by a per-tool threshold that was built for it (explicit TOOL_MIX name,
 * explicit TOOL_BUDGETS name, or a demo name on a demo server). Every other call is tagged budget=default.
 */
export function planTools(listed, cfg) {
  const byName = {};
  const listedNames = [];
  for (const t of listed || []) {
    if (!t || typeof t.name !== 'string') continue;
    byName[t.name] = t;
    listedNames.push(t.name);
  }
  const demo = isDemoServer(listedNames);
  const userArgs = cfg.toolArgs || {};
  const userBudgets = cfg.userBudgets || {};

  let mix;
  const unknown = [];
  if (cfg.toolMix) {
    mix = {};
    for (const n of Object.keys(cfg.toolMix)) {
      if (!(cfg.toolMix[n] > 0)) continue;
      if (byName[n]) mix[n] = cfg.toolMix[n];
      else unknown.push(n);
    }
  } else if (demo) {
    mix = DEMO_TOOL_MIX;
  } else {
    mix = {};
    for (const n of listedNames) mix[n] = 1;
  }

  const table = [];
  let total = 0;
  for (const name of Object.keys(mix)) {
    const w = mix[name];
    if (!(w > 0) || !byName[name]) continue;
    total += w;
    let args;
    if (userArgs[name] !== undefined) args = userArgs[name];
    else if (demo && DEMO_TOOL_ARGS[name] !== undefined) args = DEMO_TOOL_ARGS[name];
    else args = argsFromSchema(byName[name].inputSchema);
    const ownBudget =
      (!!cfg.toolMix && cfg.toolMix[name] > 0) ||
      Object.prototype.hasOwnProperty.call(userBudgets, name) ||
      (demo && DEMO_TOOLS.indexOf(name) >= 0);
    table.push({ name, cum: total, args, ownBudget });
  }
  return { table, total, demo, unknown, listedNames };
}

/** A planTools() result without the named tools (weights of the rest unchanged, `cum` recomputed). */
export function withoutTools(tt, names) {
  const table = [];
  let total = 0;
  let prev = 0;
  for (const e of tt.table) {
    const w = e.cum - prev;
    prev = e.cum;
    if (names.indexOf(e.name) >= 0) continue;
    total += w;
    table.push(Object.assign({}, e, { cum: total }));
  }
  return Object.assign({}, tt, { table, total });
}

/** Weighted random pick from a planTools() result. `rnd` in [0,1) (default Math.random()). */
export function pick(tt, rnd) {
  const r = (rnd === undefined ? Math.random() : rnd) * tt.total;
  for (const e of tt.table) if (r < e.cum) return e;
  return tt.table[tt.table.length - 1];
}
