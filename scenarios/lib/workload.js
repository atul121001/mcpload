// Pure helpers (no k6 imports) for scenarios/workload.js: the normalized workload profile, picking a flow by
// weight and a data row per session, filling {{data...}} templates, and the per-flow thresholds.
// Unit-tested by workload.test.mjs.
//
// `mcpload run --workload customer-support.yaml` validates the YAML/JSON profile (cmd/mcpload/internal/workload
// documents the file format), reads its CSV data files and writes this normalized JSON for the scenario
// (WORKLOAD_FILE):
//
//   { "name": "customer-support", "description"?, "agents"?: 20, "duration"?: "2m", "thinkMs"?: 500,
//     "data": { "customers": { "pick": "random" | "sequential", "rows": [{ "id": "C-1001", ... }, ...] } },
//     "flows": [ { "name": "lookup-orders", "weight": 60, "thinkMs"?: 800,
//                  "budgets": { "p95": 3000, "p99"?: 5000, "completion": 0.99, "steps"?: { "<step>": { "p95"?, "p99"? } } },
//                  "steps": [ <a lib/workflow.js step: { name, thinkMs?, calls: [{ tool, args, as?, repeat, forEach? }] }> ] } ] }
//
// Strings in call args may hold {{data.<pool>.<column>}} ({{data.<pool>}} for a pool of plain values): the
// whole string "{{data.p.c}}" becomes the row's value as is, a template inside a longer string is replaced by
// the value's text. `$from` references work as in agent-workflow (lib/workflow.js).
import { normalizeWorkflow, isRef } from './workflow.js';

const NAME_RE = /^[A-Za-z0-9_.-]+$/; // flow names become k6 tag values in threshold selectors
const TEMPLATE_RE = /\{\{\s*(.*?)\s*\}\}/g;
const DATA_REF_RE = /^data\.([A-Za-z_][A-Za-z0-9_]*)(?:\.([A-Za-z0-9_]+))?$/;

function isObj(x) {
  return x !== null && typeof x === 'object' && !Array.isArray(x);
}

const posNum = (v) => typeof v === 'number' && Number.isFinite(v) && v > 0;

/** Every {{...}} template in a string as [{ text, pool, column? }]; throws on one that is not data.<pool>[.<column>]. */
export function templatesIn(s) {
  const out = [];
  const re = new RegExp(TEMPLATE_RE.source, 'g');
  let m;
  while ((m = re.exec(String(s))) !== null) {
    const ref = DATA_REF_RE.exec(m[1]);
    if (!ref) throw new Error(`unknown template ${m[0]} (use {{data.<pool>.<column>}})`);
    out.push({ text: m[0], pool: ref[1], column: ref[2] });
  }
  return out;
}

function checkTemplates(v, where, data) {
  if (isRef(v)) return;
  if (Array.isArray(v)) v.forEach((x, i) => checkTemplates(x, `${where}[${i}]`, data));
  else if (isObj(v)) for (const k of Object.keys(v)) checkTemplates(v[k], `${where}.${k}`, data);
  else if (typeof v === 'string') {
    let ts;
    try {
      ts = templatesIn(v);
    } catch (e) {
      throw new Error(`${where}: ${e.message}`);
    }
    for (const t of ts) if (!data[t.pool]) throw new Error(`${where}: ${t.text} names data pool '${t.pool}', which the workload does not define`);
  }
}

/**
 * Validate a normalized workload (as written by mcpload) and return it with defaults filled in. Throws with the
 * flow/step at fault. The CLI already validated the source file; this catches hand-written or stale JSON.
 */
export function normalizeWorkload(raw) {
  if (!isObj(raw)) throw new Error('WORKLOAD_FILE must hold a JSON object (run it with mcpload run --workload <file.yaml>)');
  if (typeof raw.name !== 'string' || !raw.name) throw new Error('workload: name must be a non-empty string');
  if (raw.agents !== undefined && !(Number.isInteger(raw.agents) && raw.agents >= 1)) throw new Error('workload: agents must be an integer >= 1');
  if (raw.thinkMs !== undefined && !(typeof raw.thinkMs === 'number' && raw.thinkMs >= 0)) throw new Error('workload: thinkMs must be a number >= 0');
  const data = {};
  for (const name of Object.keys(raw.data || {})) {
    const p = raw.data[name];
    if (!isObj(p) || !Array.isArray(p.rows) || !p.rows.length) throw new Error(`workload data.${name}: needs a non-empty "rows" array`);
    const pick = p.pick === undefined ? 'random' : p.pick;
    if (pick !== 'random' && pick !== 'sequential') throw new Error(`workload data.${name}: pick must be random or sequential`);
    data[name] = { pick, rows: p.rows };
  }
  if (!Array.isArray(raw.flows) || !raw.flows.length) throw new Error('workload: needs a non-empty "flows" array');
  const names = [];
  const flows = raw.flows.map((f, i) => {
    const where = `workload flow ${i + 1}`;
    if (!isObj(f)) throw new Error(`${where} must be an object`);
    if (typeof f.name !== 'string' || !NAME_RE.test(f.name)) throw new Error(`${where}: name must match ${NAME_RE} (got '${f.name}')`);
    const fw = `workload flow '${f.name}'`;
    if (names.indexOf(f.name) >= 0) throw new Error(`${fw}: duplicate flow name`);
    names.push(f.name);
    if (!posNum(f.weight)) throw new Error(`${fw}: weight must be a number > 0`);
    if (f.thinkMs !== undefined && !(typeof f.thinkMs === 'number' && f.thinkMs >= 0)) throw new Error(`${fw}: thinkMs must be a number >= 0`);
    let plan;
    try {
      plan = normalizeWorkflow({ steps: f.steps });
    } catch (e) {
      throw new Error(`${fw}: ${e.message.replace(/^WORKFLOW /, '')}`);
    }
    for (const st of plan.steps) st.calls.forEach((c, j) => checkTemplates(c.args, `${fw} step '${st.name}' call ${j + 1} (${c.tool}) args`, data));
    const b = f.budgets || {};
    if (!posNum(b.p95)) throw new Error(`${fw}: budgets.p95 must be a number > 0`);
    if (b.p99 !== undefined && !posNum(b.p99)) throw new Error(`${fw}: budgets.p99 must be a number > 0`);
    if (!(typeof b.completion === 'number' && b.completion > 0 && b.completion <= 1)) throw new Error(`${fw}: budgets.completion must be in (0, 1]`);
    const steps = {};
    for (const sn of Object.keys(b.steps || {})) {
      if (!plan.steps.some((st) => st.name === sn)) throw new Error(`${fw}: budgets.steps names no step '${sn}'`);
      const sb = b.steps[sn] || {};
      for (const k of ['p95', 'p99']) if (sb[k] !== undefined && !posNum(sb[k])) throw new Error(`${fw}: budgets.steps.${sn}.${k} must be a number > 0`);
      steps[sn] = sb;
    }
    const out = { name: f.name, weight: f.weight, steps: plan.steps, budgets: { p95: b.p95, p99: b.p99, completion: b.completion, steps } };
    if (f.thinkMs !== undefined) out.thinkMs = f.thinkMs;
    return out;
  });
  const out = { name: raw.name, data, flows };
  for (const k of ['description', 'agents', 'duration', 'thinkMs']) if (raw[k] !== undefined) out[k] = raw[k];
  return out;
}

/** Pick a flow by weight. `rnd` in [0,1). */
export function pickFlow(flows, rnd) {
  let total = 0;
  for (const f of flows) total += f.weight;
  let r = (rnd === undefined ? Math.random() : rnd) * total;
  for (const f of flows) {
    if (r < f.weight) return f;
    r -= f.weight;
  }
  return flows[flows.length - 1];
}

/**
 * One row per data pool for a session: { <pool>: row }. A sequential pool takes row `seq` modulo its length
 * (seq: the iteration number across all VUs), a random one a uniform pick using rnd() in [0,1).
 */
export function pickRows(data, seq, rnd) {
  const out = {};
  for (const name of Object.keys(data)) {
    const p = data[name];
    const n = p.rows.length;
    const i = p.pick === 'sequential' ? ((seq % n) + n) % n : Math.min(n - 1, Math.floor((rnd ? rnd() : Math.random()) * n));
    out[name] = p.rows[i];
  }
  return out;
}

function templateValue(t, rows) {
  const row = rows[t.pool];
  if (t.column === undefined) return row;
  return row !== null && typeof row === 'object' ? row[t.column] : undefined;
}

/**
 * Fill {{data...}} templates anywhere in v (references are left alone). A string that is exactly one template
 * becomes the value itself (a number stays a number); otherwise each template is replaced by the value's text.
 */
export function renderTemplates(v, rows) {
  if (isRef(v)) return v;
  if (Array.isArray(v)) return v.map((x) => renderTemplates(x, rows));
  if (isObj(v)) {
    const o = {};
    for (const k of Object.keys(v)) o[k] = renderTemplates(v[k], rows);
    return o;
  }
  if (typeof v !== 'string' || v.indexOf('{{') < 0) return v;
  const ts = templatesIn(v);
  if (ts.length === 1 && ts[0].text === v) return templateValue(ts[0], rows);
  return v.replace(TEMPLATE_RE, (m) => {
    const t = templatesIn(m)[0];
    const x = templateValue(t, rows);
    if (x === undefined || x === null) return '';
    return typeof x === 'object' ? JSON.stringify(x) : String(x);
  });
}

/** A flow's steps with the session's data filled into every call's args. */
export function renderSteps(steps, rows) {
  return steps.map((st) => Object.assign({}, st, { calls: st.calls.map((c) => Object.assign({}, c, { args: renderTemplates(c.args, rows) })) }));
}

/** Unique tool names over every flow, in order of first use. */
export function workloadToolNames(wl) {
  const names = [];
  for (const f of wl.flows) for (const st of f.steps) for (const c of st.calls) if (names.indexOf(c.tool) < 0) names.push(c.tool);
  return names;
}

/**
 * k6 thresholds for every flow's budgets: end-to-end p95 (and p99), completion rate, and each step budget.
 * Metrics: mcp_workload_flow_duration{flow}, mcp_workload_flow_complete{flow}, mcp_workload_step_duration{flow,flow_step}.
 */
export function workloadThresholds(wl) {
  const t = {};
  const lat = (b) => (b.p95 !== undefined ? [`p(95)<${b.p95}`] : []).concat(b.p99 !== undefined ? [`p(99)<${b.p99}`] : []);
  for (const f of wl.flows) {
    t[`mcp_workload_flow_duration{flow:${f.name}}`] = lat(f.budgets);
    t[`mcp_workload_flow_complete{flow:${f.name}}`] = [`rate>=${f.budgets.completion}`];
    for (const sn of Object.keys(f.budgets.steps || {})) {
      const l = lat(f.budgets.steps[sn]);
      if (l.length) t[`mcp_workload_step_duration{flow:${f.name},flow_step:${sn}}`] = l;
    }
  }
  return t;
}
