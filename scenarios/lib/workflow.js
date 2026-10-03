// Pure helpers (no k6 imports) for scenarios/agent-workflow.js: the WORKFLOW plan format, its validation,
// pulling values out of earlier tool results into later calls' arguments, and the workflow thresholds.
// Unit-tested by workflow.test.mjs.
//
// A plan is a list of steps run in order. All calls of one step are sent together (callParallel), so a step
// lasts as long as its slowest call. Between steps the agent "thinks" (exponential pause, mean THINK_MS or the
// step's thinkMs). A call can name its result (`as`) and a later step can use it:
//
//   { "steps": [
//       { "name": "gather", "calls": [
//           { "tool": "search", "args": { "query": "invoices" }, "as": "hits" },
//           { "tool": "fast", "repeat": 2 } ] },
//       { "name": "open", "thinkMs": 800, "calls": [
//           { "tool": "search", "forEach": { "$from": "hits", "path": "results", "max": 3 },
//             "args": { "query": { "$from": "$item", "path": "title" } } } ] } ] }
//
// A step:   { name?, thinkMs?, calls: [call, ...] }           name defaults to step1, step2, ...
// A call:   { tool, args?, as?, repeat?, forEach? }
//   args     merged over the tool's base args (TOOL_ARGS, else the demo args on a demo server, else placeholders
//            from its inputSchema). Any value, at any depth, may be a reference (below).
//   as       name for this call's result, usable by later steps. With repeat or forEach it is the list of the
//            successful results.
//   repeat   send N identical calls in this step (static fan-out width, default 1).
//   forEach  { $from, path?, match?, max? }: one call per element of a list taken from an earlier result
//            (data-dependent fan-out, at most `max`, default 5). Inside its args, { "$from": "$item", ... }
//            refers to the current element.
// A reference: { "$from": "<as name>", "path"?: "results.0.title", "match"?: "<regex>" }
//   The named result is read as its structuredContent if present, else its first text content parsed as JSON,
//   else that text. `path` walks it (dots or [n]); `match` runs a regex on the value (stringified if not a
//   string) and yields the first capture group, or the whole match.

export const DEFAULT_FOREACH_MAX = 5;

/**
 * Default plan for the demo servers (fast, slow, flaky, big, search; search returns
 * {"query", "results": [{title, url, score}]} as JSON text). 4 steps, 11 calls, 3 think pauses.
 */
export const DEFAULT_WORKFLOW = {
  steps: [
    {
      // Independent lookups fired together.
      name: 'gather',
      calls: [
        { tool: 'search', args: { query: 'overdue invoices', limit: 5 }, as: 'invoices' },
        { tool: 'search', args: { query: 'customer accounts', limit: 3 }, as: 'accounts' },
        { tool: 'fast' },
      ],
    },
    {
      // Open the top invoice hits, one call per hit, while a slow lookup runs alongside.
      name: 'inspect',
      calls: [
        {
          tool: 'search',
          forEach: { $from: 'invoices', path: 'results', max: 3 },
          args: { query: { $from: '$item', path: 'title' }, limit: 2 },
          as: 'details',
        },
        { tool: 'slow', args: { ms: 200 } },
      ],
    },
    {
      // Act on what was found: a lookup built from step 2's first result, plus unrelated calls.
      name: 'act',
      calls: [
        { tool: 'search', args: { query: { $from: 'details', path: '0.results.0.title' }, limit: 1 }, as: 'record' },
        { tool: 'big' },
        { tool: 'flaky' },
      ],
    },
    {
      // Final call: an id pulled out of step 3's result text with a regex.
      name: 'report',
      calls: [{ tool: 'search', args: { query: { $from: 'record', path: 'results.0.url', match: '/search/([^/]+)/' }, limit: 1 } }],
    },
  ],
};

const NAME_RE = /^[A-Za-z0-9_.-]+$/; // step names become k6 tag values in threshold selectors
const ITEM = '$item';

function isObj(x) {
  return x !== null && typeof x === 'object' && !Array.isArray(x);
}

/** True for a reference object: a plain object with a `$from` key. */
export function isRef(v) {
  return isObj(v) && Object.prototype.hasOwnProperty.call(v, '$from');
}

function checkRef(ref, where, known, allowItem) {
  if (typeof ref.$from !== 'string' || !ref.$from) throw new Error(`${where}: $from must be a non-empty string`);
  for (const k of Object.keys(ref)) {
    if (['$from', 'path', 'match', 'max'].indexOf(k) < 0) throw new Error(`${where}: unknown reference key '${k}'`);
  }
  if (ref.path !== undefined && typeof ref.path !== 'string') throw new Error(`${where}: path must be a string`);
  if (ref.match !== undefined) {
    if (typeof ref.match !== 'string') throw new Error(`${where}: match must be a string`);
    try {
      new RegExp(ref.match);
    } catch (e) {
      throw new Error(`${where}: match is not a valid regex: ${e.message}`);
    }
  }
  if (ref.$from === ITEM) {
    if (!allowItem) throw new Error(`${where}: $item is only available inside a call with forEach`);
  } else if (known.indexOf(ref.$from) < 0) {
    throw new Error(`${where}: '${ref.$from}' is not the 'as' name of a call in an earlier step`);
  }
}

function checkArgRefs(v, where, known, allowItem) {
  if (isRef(v)) {
    if (v.max !== undefined) throw new Error(`${where}: max is only valid in forEach`);
    return checkRef(v, where, known, allowItem);
  }
  if (Array.isArray(v)) v.forEach((x, i) => checkArgRefs(x, `${where}[${i}]`, known, allowItem));
  else if (isObj(v)) for (const k of Object.keys(v)) checkArgRefs(v[k], `${where}.${k}`, known, allowItem);
}

/**
 * Validate a WORKFLOW value (an object with `steps`, or the steps array itself) and return a normalized plan
 * { steps: [{ name, thinkMs?, calls: [{ tool, args, as?, repeat, forEach? }] }] }. Throws with the location of
 * the first problem (unknown keys, a reference to a name no earlier step defines, duplicate names, ...).
 */
export function normalizeWorkflow(raw) {
  const stepsIn = Array.isArray(raw) ? raw : isObj(raw) ? raw.steps : undefined;
  if (!Array.isArray(stepsIn) || !stepsIn.length) throw new Error('WORKFLOW needs a non-empty "steps" array');
  const known = []; // `as` names from earlier steps
  const stepNames = [];
  const steps = stepsIn.map((st, i) => {
    const where = `WORKFLOW step ${i + 1}`;
    if (!isObj(st)) throw new Error(`${where} must be an object`);
    for (const k of Object.keys(st)) {
      if (['name', 'thinkMs', 'calls'].indexOf(k) < 0) throw new Error(`${where}: unknown key '${k}'`);
    }
    const name = st.name === undefined ? `step${i + 1}` : st.name;
    if (typeof name !== 'string' || !NAME_RE.test(name)) throw new Error(`${where}: name must match ${NAME_RE} (got '${name}')`);
    if (stepNames.indexOf(name) >= 0) throw new Error(`${where}: duplicate step name '${name}'`);
    stepNames.push(name);
    if (st.thinkMs !== undefined && !(typeof st.thinkMs === 'number' && st.thinkMs >= 0)) {
      throw new Error(`${where} (${name}): thinkMs must be a number >= 0`);
    }
    if (!Array.isArray(st.calls) || !st.calls.length) throw new Error(`${where} (${name}): needs a non-empty "calls" array`);
    const added = [];
    const calls = st.calls.map((c, j) => {
      const cw = `${where} (${name}) call ${j + 1}`;
      if (!isObj(c)) throw new Error(`${cw} must be an object`);
      for (const k of Object.keys(c)) {
        if (['tool', 'args', 'as', 'repeat', 'forEach'].indexOf(k) < 0) throw new Error(`${cw}: unknown key '${k}'`);
      }
      if (typeof c.tool !== 'string' || !c.tool) throw new Error(`${cw}: tool must be a non-empty string`);
      if (c.args !== undefined && !isObj(c.args)) throw new Error(`${cw} (${c.tool}): args must be an object`);
      const repeat = c.repeat === undefined ? 1 : c.repeat;
      if (!Number.isInteger(repeat) || repeat < 1) throw new Error(`${cw} (${c.tool}): repeat must be an integer >= 1`);
      let forEach;
      if (c.forEach !== undefined) {
        if (!isRef(c.forEach)) throw new Error(`${cw} (${c.tool}): forEach must be a reference {"$from": ...}`);
        if (repeat !== 1) throw new Error(`${cw} (${c.tool}): use either repeat or forEach, not both`);
        checkRef(c.forEach, `${cw} (${c.tool}) forEach`, known, false);
        const max = c.forEach.max === undefined ? DEFAULT_FOREACH_MAX : c.forEach.max;
        if (!Number.isInteger(max) || max < 1) throw new Error(`${cw} (${c.tool}): forEach.max must be an integer >= 1`);
        forEach = Object.assign({}, c.forEach, { max });
      }
      const args = c.args || {};
      checkArgRefs(args, `${cw} (${c.tool}) args`, known, !!forEach);
      if (c.as !== undefined) {
        if (typeof c.as !== 'string' || !c.as || c.as === ITEM) throw new Error(`${cw} (${c.tool}): as must be a non-empty string other than $item`);
        if (known.indexOf(c.as) >= 0 || added.indexOf(c.as) >= 0) throw new Error(`${cw} (${c.tool}): duplicate as name '${c.as}'`);
        added.push(c.as);
      }
      const out = { tool: c.tool, args, repeat };
      if (c.as !== undefined) out.as = c.as;
      if (forEach) out.forEach = forEach;
      return out;
    });
    // Names defined in this step only become usable in later steps: calls of one step run in parallel.
    for (const a of added) known.push(a);
    const out = { name, calls };
    if (st.thinkMs !== undefined) out.thinkMs = st.thinkMs;
    return out;
  });
  return { steps };
}

/** Unique tool names a plan calls, in order of first use. */
export function planToolNames(plan) {
  const names = [];
  for (const st of plan.steps) for (const c of st.calls) if (names.indexOf(c.tool) < 0) names.push(c.tool);
  return names;
}

/** Tools the plan needs that the server does not list. */
export function missingTools(plan, listedNames) {
  return planToolNames(plan).filter((n) => listedNames.indexOf(n) < 0);
}

/**
 * The value a later step reads from a tool result: structuredContent if present, else the first text content
 * parsed as JSON, else that text, else the raw content array.
 */
export function resultView(res) {
  if (!res) return undefined;
  if (res.structuredContent !== undefined && res.structuredContent !== null) return res.structuredContent;
  const content = Array.isArray(res.content) ? res.content : [];
  const t = content.find((c) => c && c.type === 'text' && typeof c.text === 'string');
  if (!t) return content;
  try {
    return JSON.parse(t.text);
  } catch (e) {
    return t.text;
  }
}

/** Walk `path` ("a.b.0.c" or "a.b[0].c"; empty = the value itself). Undefined when any segment is missing. */
export function getPath(value, path) {
  if (path === undefined || path === '') return value;
  const segs = String(path)
    .replace(/\[(\d+)\]/g, '.$1')
    .split('.')
    .filter((s) => s !== '');
  let cur = value;
  for (const s of segs) {
    if (cur === null || typeof cur !== 'object' || !(s in cur)) return undefined;
    cur = cur[s];
  }
  return cur;
}

/**
 * A scope holds the results earlier steps named with `as`: values (result views) and the names whose calls
 * all failed. resolveRef(ref, scope) returns { ok: true, value } or { ok: false, reason, detail } where
 * reason is 'failed' (the source call failed: a tool error, not a plan problem) or 'missing' (the source
 * succeeded but has nothing at `path`, or `match` did not match: the plan does not fit this server).
 */
export function newScope() {
  return { values: {}, failed: {} };
}

export function resolveRef(ref, scope) {
  const name = ref.$from;
  if (scope.failed[name]) return { ok: false, reason: 'failed', detail: `'${name}' failed` };
  if (!Object.prototype.hasOwnProperty.call(scope.values, name)) return { ok: false, reason: 'missing', detail: `'${name}' has no result` };
  let v = getPath(scope.values[name], ref.path);
  const where = ref.path ? `${name}.${ref.path}` : name;
  if (v === undefined) return { ok: false, reason: 'missing', detail: `${where} not found` };
  if (ref.match !== undefined) {
    const m = new RegExp(ref.match).exec(typeof v === 'string' ? v : JSON.stringify(v));
    if (!m) return { ok: false, reason: 'missing', detail: `${where} does not match /${ref.match}/` };
    v = m.length > 1 && m[1] !== undefined ? m[1] : m[0];
  }
  return { ok: true, value: v };
}

/** Replace every reference inside `v`. Returns { value, problems: [{reason, detail}] }. */
export function resolveArgs(v, scope) {
  const problems = [];
  const walk = (x) => {
    if (isRef(x)) {
      const r = resolveRef(x, scope);
      if (!r.ok) {
        problems.push({ reason: r.reason, detail: r.detail });
        return undefined;
      }
      return r.value;
    }
    if (Array.isArray(x)) return x.map(walk);
    if (isObj(x)) {
      const o = {};
      for (const k of Object.keys(x)) o[k] = walk(x[k]);
      return o;
    }
    return x;
  };
  return { value: walk(v), problems };
}

/** True when an args object (possibly nested) contains a reference. */
export function hasRefs(v) {
  if (isRef(v)) return true;
  if (Array.isArray(v)) return v.some(hasRefs);
  if (isObj(v)) return Object.keys(v).some((k) => hasRefs(v[k]));
  return false;
}

/**
 * Turn one plan step into concrete calls. baseArgs(tool) gives the tool's default args, which the plan's args
 * are shallow-merged over. Returns { calls: [{ name, args, call }], problems, hasRefs } where `call` is the
 * index of the plan call each concrete call came from (repeat and forEach expand to several).
 */
export function expandStep(step, scope, baseArgs) {
  const calls = [];
  const problems = [];
  let refs = false;
  step.calls.forEach((c, ci) => {
    let items = [undefined];
    if (c.forEach) {
      refs = true;
      const r = resolveRef(c.forEach, scope);
      if (!r.ok) {
        problems.push({ reason: r.reason, detail: r.detail });
        return;
      }
      items = (Array.isArray(r.value) ? r.value : [r.value]).slice(0, c.forEach.max);
    } else if (c.repeat > 1) {
      items = new Array(c.repeat).fill(undefined);
    }
    if (hasRefs(c.args)) refs = true;
    for (const item of items) {
      let sc = scope;
      if (c.forEach) {
        sc = { values: Object.assign({}, scope.values), failed: scope.failed };
        sc.values[ITEM] = item;
      }
      const r = resolveArgs(c.args, sc);
      if (r.problems.length) {
        for (const p of r.problems) problems.push(p);
        continue;
      }
      calls.push({ name: c.tool, args: Object.assign({}, baseArgs ? baseArgs(c.tool) : {}, r.value), call: ci });
    }
  });
  return { calls, problems, hasRefs: refs };
}

/** A call failed: transport/protocol error or isError: true. */
export function failed(res) {
  return !res || !!res.error || !!res.isError;
}

/**
 * Store a step's results in the scope under each call's `as` name. A plain call stores its result view; repeat
 * and forEach store the list of successful views. A name is marked failed when none of its calls succeeded.
 */
export function recordResults(step, calls, results, scope) {
  step.calls.forEach((c, ci) => {
    if (c.as === undefined) return;
    const mine = [];
    for (let i = 0; i < calls.length; i++) if (calls[i].call === ci) mine.push(results[i]);
    const ok = mine.filter((r) => !failed(r));
    const list = c.forEach || c.repeat > 1;
    // forEach over an empty list ran no calls: an empty list, not a failure.
    if (!ok.length && !(list && !mine.length)) {
      scope.failed[c.as] = true;
      delete scope.values[c.as];
      return;
    }
    delete scope.failed[c.as];
    scope.values[c.as] = list ? ok.map(resultView) : resultView(ok[0]);
  });
}

/** Think time in seconds: exponential with mean `meanMs`, capped at 5x the mean. `rnd` in [0,1). */
export function thinkSecondsFor(meanMs, rnd) {
  if (!(meanMs > 0)) return 0;
  const x = -Math.log(1 - (rnd === undefined ? Math.random() : rnd)) * meanMs;
  return Math.min(x, meanMs * 5) / 1000;
}

/**
 * k6 thresholds for the workflow metrics.
 * b: { workflowP95, workflowP99?, stepP95, stepP99?, stepBudgets: {<step>: {p95?, p99?}}, minComplete }
 */
export function workflowThresholds(plan, b) {
  const t = {
    mcp_workflow_duration: [`p(95)<${b.workflowP95}`].concat(b.workflowP99 !== undefined ? [`p(99)<${b.workflowP99}`] : []),
    mcp_workflow_complete: [`rate>=${b.minComplete}`],
  };
  for (const st of plan.steps) {
    const sb = Object.assign({ p95: b.stepP95, p99: b.stepP99 }, (b.stepBudgets || {})[st.name] || {});
    t[`mcp_workflow_step_duration{step:${st.name}}`] = [`p(95)<${sb.p95}`].concat(sb.p99 !== undefined ? [`p(99)<${sb.p99}`] : []);
  }
  return t;
}
