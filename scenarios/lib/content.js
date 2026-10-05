// Pure helpers (no k6 imports) for mixing resource reads and prompt gets into agent sessions
// (RESOURCE_READ_RATIO / PROMPT_GET_RATIO): what a session can read or get, and what each step does.
// Unit-tested by content.test.mjs.

/**
 * Parse the content knobs.
 *   RESOURCE_READ_RATIO  share of agent steps that are a resources/read instead of a tools/call, 0..1 (default 0)
 *   PROMPT_GET_RATIO     share of agent steps that are a prompts/get instead of a tools/call, 0..1 (default 0)
 * The two together must not exceed 1. `get(name)` returns the raw value or undefined.
 * Returns { resourceRatio, promptRatio }.
 */
export function parseContentConfig(get) {
  const ratio = (name) => {
    const v = get(name);
    if (v === undefined || v === '') return 0;
    const n = Number(v);
    if (!Number.isFinite(n)) throw new Error(`${name} must be a number, got '${v}'`);
    if (n < 0 || n > 1) throw new Error(`${name} must be between 0 and 1, got '${v}'`);
    return n;
  };
  const resourceRatio = ratio('RESOURCE_READ_RATIO');
  const promptRatio = ratio('PROMPT_GET_RATIO');
  if (resourceRatio + promptRatio > 1 + 1e-9) {
    throw new Error(`RESOURCE_READ_RATIO + PROMPT_GET_RATIO must not exceed 1, got ${resourceRatio} + ${promptRatio}`);
  }
  return { resourceRatio, promptRatio };
}

/** True when a session should list resources/prompts at all. */
export function contentEnabled(cc) {
  return !!cc && (cc.resourceRatio > 0 || cc.promptRatio > 0);
}

/** A small random id ("1".."999") for template variables. `rnd` in [0,1). */
export function randomId(rnd) {
  const r = rnd === undefined ? Math.random() : rnd;
  return String(1 + Math.min(998, Math.floor(r * 999)));
}

/**
 * Expand an RFC 6570 URI template with a random id for every variable, e.g. demo://items/{id} ->
 * demo://items/417. Operators: {x} {+x} {#x} {.x} {/x} {;x} {?x,y} {&x}; modifiers (:3, *) are ignored.
 * `rnd()` in [0,1) (default Math.random).
 */
export function fillTemplate(tpl, rnd) {
  const next = rnd || Math.random;
  return String(tpl).replace(/\{([+#./;?&]?)([^}]*)\}/g, (_, op, list) => {
    const names = list
      .split(',')
      .map((v) => v.trim().replace(/[:*].*$/, ''))
      .filter((v) => v !== '');
    const vals = names.map((n) => ({ n, v: encodeURIComponent(randomId(next())) }));
    switch (op) {
      case '':
      case '+':
        return vals.map((x) => x.v).join(',');
      case '#':
        return '#' + vals.map((x) => x.v).join(',');
      case '.':
        return vals.map((x) => '.' + x.v).join('');
      case '/':
        return vals.map((x) => '/' + x.v).join('');
      case ';':
        return vals.map((x) => `;${x.n}=${x.v}`).join('');
      case '?':
        return vals.length ? '?' + vals.map((x) => `${x.n}=${x.v}`).join('&') : '';
      default: // '&'
        return vals.map((x) => `&${x.n}=${x.v}`).join('');
    }
  });
}

/**
 * What a session can read and get, from resources/list, resources/templates/list and prompts/list (any may be
 * undefined when the list failed or was not asked for).
 * Returns { resources: [{uri}|{uriTemplate}], prompts: [{name, required: string[]}] }.
 */
export function planContent(resources, templates, prompts) {
  const rs = [];
  for (const r of resources || []) if (r && typeof r.uri === 'string' && r.uri) rs.push({ uri: r.uri });
  for (const t of templates || []) if (t && typeof t.uriTemplate === 'string' && t.uriTemplate) rs.push({ uriTemplate: t.uriTemplate });
  const ps = [];
  for (const p of prompts || []) {
    if (!p || typeof p.name !== 'string' || !p.name) continue;
    const required = [];
    for (const a of p.arguments || []) if (a && a.required && typeof a.name === 'string') required.push(a.name);
    ps.push({ name: p.name, required });
  }
  return { resources: rs, prompts: ps };
}

/** Prompt arguments: every required one gets a short string; optional ones are left out. */
export function promptArgs(p, rnd) {
  const out = {};
  for (const n of p.required) out[n] = `test ${randomId(rnd ? rnd() : undefined)}`;
  return out;
}

/**
 * The kind of one agent step: 'resource' with probability resourceRatio, 'prompt' with promptRatio, else
 * 'tool'. A kind the server listed nothing for falls back to 'tool'. `rnd` in [0,1).
 */
export function stepKind(cc, plan, rnd) {
  if (!contentEnabled(cc) || !plan) return 'tool';
  const r = rnd === undefined ? Math.random() : rnd;
  if (r < cc.resourceRatio) return plan.resources.length ? 'resource' : 'tool';
  if (r < cc.resourceRatio + cc.promptRatio) return plan.prompts.length ? 'prompt' : 'tool';
  return 'tool';
}

/**
 * One batch entry for a resource read or prompt get, uniform over what was listed:
 * { kind: 'resource', uri } or { kind: 'prompt', name, args }.
 * `rnd()` in [0,1) (default Math.random).
 */
export function pickContent(plan, kind, rnd) {
  const next = rnd || Math.random;
  const pickOne = (xs) => xs[Math.min(xs.length - 1, Math.floor(next() * xs.length))];
  if (kind === 'resource') {
    const r = pickOne(plan.resources);
    const uri = r.uri !== undefined ? r.uri : fillTemplate(r.uriTemplate, next);
    return { kind: 'resource', uri };
  }
  const p = pickOne(plan.prompts);
  return { kind: 'prompt', name: p.name, args: promptArgs(p, next) };
}

/** A log label for a batch entry: the tool name, `resources/read <uri>` or `prompts/get <name>`. */
export function entryLabel(e) {
  if (e.kind === 'resource') return `resources/read ${e.uri}`;
  if (e.kind === 'prompt') return `prompts/get ${e.name}`;
  return e.name;
}

/**
 * The callParallel item for a batch entry (a tool entry from lib/tools.js#pick or one from pickContent),
 * with the per-call options `co` ({meta?, cancelAfterMs?}) merged in.
 */
export function parallelItem(e, co) {
  let item;
  if (e.kind === 'resource') item = { kind: 'resource', uri: e.uri };
  else if (e.kind === 'prompt') item = { kind: 'prompt', name: e.name, args: e.args };
  else item = { name: e.name, args: e.args };
  return Object.assign(item, co || {});
}
