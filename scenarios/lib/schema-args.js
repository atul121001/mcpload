// Pure helpers (no k6 imports) so they can be unit-tested with plain Node: see schema-args.test.mjs.
//
// argsFromSchema(inputSchema) builds placeholder arguments for a tool from its JSON Schema: every
// required property (recursively, for nested objects) gets a value that should pass validation.
// Supported: const, default, enum, examples[0], anyOf/oneOf (first non-null branch), allOf (merged),
// type arrays, nested objects, arrays with minItems (items / prefixItems / tuple items, uniqueItems),
// string format (uri, url, uri-reference, email, uuid, date, date-time, time, hostname, ipv4, ipv6),
// minLength/maxLength, number minimum/maximum/exclusiveMinimum/exclusiveMaximum/multipleOf, and local
// $ref (#/$defs/..., #/definitions/..., any #/json/pointer). Unresolvable or cyclic schemas fall back to
// a plain string. Optional properties are left out.

const MAX_DEPTH = 12;
const PLACEHOLDER = 'load-test';

const FORMATS = {
  uri: 'https://example.com/load-test',
  url: 'https://example.com/load-test',
  'uri-reference': 'https://example.com/load-test',
  iri: 'https://example.com/load-test',
  email: 'load-test@example.com',
  'idn-email': 'load-test@example.com',
  uuid: '00000000-0000-4000-8000-000000000000',
  date: '2026-01-01',
  'date-time': '2026-01-01T00:00:00Z',
  time: '00:00:00Z',
  hostname: 'example.com',
  ipv4: '127.0.0.1',
  ipv6: '::1',
};

function isObj(x) {
  return x !== null && typeof x === 'object' && !Array.isArray(x);
}

function resolvePointer(root, ref) {
  if (typeof ref !== 'string' || ref.charAt(0) !== '#') return undefined;
  if (ref === '#') return root;
  if (ref.charAt(1) !== '/') return undefined;
  let cur = root;
  for (const raw of ref.slice(2).split('/')) {
    const key = decodeURIComponent(raw).replace(/~1/g, '/').replace(/~0/g, '~');
    if (cur === null || typeof cur !== 'object' || !(key in cur)) return undefined;
    cur = cur[key];
  }
  return cur;
}

/** Resolve $ref (siblings win over the target), allOf (merged), and the first non-null anyOf/oneOf branch. */
function normalize(schema, root, depth) {
  let s = schema;
  for (let guard = 0; guard < MAX_DEPTH && isObj(s); guard++) {
    if (typeof s.$ref === 'string') {
      const target = resolvePointer(root, s.$ref);
      const rest = Object.assign({}, s);
      delete rest.$ref;
      s = isObj(target) ? Object.assign({}, target, rest) : rest;
      continue;
    }
    if (Array.isArray(s.allOf) && s.allOf.length) {
      const rest = Object.assign({}, s);
      delete rest.allOf;
      const merged = { properties: {}, required: [] };
      for (const part of s.allOf.concat([rest])) {
        const p = normalize(part, root, depth + 1);
        if (!isObj(p)) continue;
        Object.assign(merged.properties, p.properties || {});
        merged.required = merged.required.concat(p.required || []);
        for (const k of Object.keys(p)) if (k !== 'properties' && k !== 'required' && merged[k] === undefined) merged[k] = p[k];
      }
      if (!Object.keys(merged.properties).length) delete merged.properties;
      if (!merged.required.length) delete merged.required;
      s = merged;
      continue;
    }
    const alts = Array.isArray(s.anyOf) && s.anyOf.length ? s.anyOf : Array.isArray(s.oneOf) && s.oneOf.length ? s.oneOf : null;
    if (alts) {
      const branch = alts.find((b) => !(isObj(b) && b.type === 'null')) || alts[0];
      const rest = Object.assign({}, s);
      delete rest.anyOf;
      delete rest.oneOf;
      s = Object.assign({}, rest, isObj(branch) ? branch : {});
      continue;
    }
    break;
  }
  return s;
}

function typeOf(s) {
  let t = s.type;
  if (Array.isArray(t)) t = t.find((x) => x !== 'null') || t[0];
  if (typeof t === 'string') return t;
  if (isObj(s.properties) || Array.isArray(s.required)) return 'object';
  if (s.items !== undefined || s.prefixItems !== undefined || s.minItems !== undefined) return 'array';
  if (s.format !== undefined || s.minLength !== undefined || s.maxLength !== undefined || s.pattern !== undefined) return 'string';
  if (s.minimum !== undefined || s.maximum !== undefined || s.exclusiveMinimum !== undefined || s.multipleOf !== undefined) return 'number';
  return 'string';
}

function roundFloat(x) {
  return Number(x.toFixed(10));
}

function numberFor(s, integer) {
  const step = typeof s.multipleOf === 'number' && s.multipleOf > 0 ? s.multipleOf : integer ? 1 : 0;
  let lo;
  let loExclusive = false;
  if (typeof s.exclusiveMinimum === 'number') {
    lo = s.exclusiveMinimum;
    loExclusive = true;
  }
  if (typeof s.minimum === 'number' && (lo === undefined || s.minimum > lo)) {
    lo = s.minimum;
    loExclusive = s.exclusiveMinimum === true; // draft-04 boolean form
  }
  let hi;
  let hiExclusive = false;
  if (typeof s.exclusiveMaximum === 'number') {
    hi = s.exclusiveMaximum;
    hiExclusive = true;
  }
  if (typeof s.maximum === 'number' && (hi === undefined || s.maximum < hi)) {
    hi = s.maximum;
    hiExclusive = s.exclusiveMaximum === true;
  }

  let v;
  if (lo === undefined) v = hi !== undefined && hi <= 1 ? (hiExclusive ? hi - (step || 1) : hi) : 1;
  else if (!loExclusive) v = lo;
  else if (step) v = lo + step;
  else v = hi !== undefined && hi - lo <= 1 ? (lo + hi) / 2 : lo + 1;

  if (integer) v = loExclusive && Number.isInteger(v) && v === lo ? v + 1 : Math.ceil(v);
  if (step) {
    let k = Math.ceil(roundFloat(v / step));
    if (loExclusive && roundFloat(k * step) <= lo) k++;
    v = roundFloat(k * step);
    if (integer && !Number.isInteger(v)) v = Math.ceil(v);
  }
  if (hi !== undefined && (v > hi || (hiExclusive && v >= hi))) {
    // Pull back inside the upper bound (best effort; contradictory schemas can't be satisfied).
    if (step) {
      let k = Math.floor(roundFloat(hi / step));
      if (hiExclusive && roundFloat(k * step) >= hi) k--;
      v = roundFloat(k * step);
    } else v = hiExclusive ? (integer ? Math.ceil(hi) - 1 : hi - (lo !== undefined ? (hi - lo) / 2 : 1)) : hi;
  }
  return v;
}

function stringFor(s) {
  const fmt = typeof s.format === 'string' ? FORMATS[s.format] : undefined;
  let v = fmt !== undefined ? fmt : PLACEHOLDER;
  if (typeof s.minLength === 'number' && v.length < s.minLength) v = v + 'x'.repeat(s.minLength - v.length);
  if (typeof s.maxLength === 'number' && v.length > s.maxLength) v = v.slice(0, Math.max(0, s.maxLength));
  return v;
}

function vary(v, i) {
  if (i === 0) return v;
  if (typeof v === 'string') return `${v}-${i}`;
  if (typeof v === 'number') return v + i;
  return v;
}

function valueFor(schema, root, depth) {
  if (schema === true || schema === undefined) return PLACEHOLDER;
  if (depth > MAX_DEPTH) return PLACEHOLDER;
  const s = normalize(schema, root, depth);
  if (!isObj(s)) return PLACEHOLDER;

  if (s.const !== undefined) return s.const;
  if (s.default !== undefined) return s.default;
  if (Array.isArray(s.enum) && s.enum.length) return s.enum[0];
  if (Array.isArray(s.examples) && s.examples.length) return s.examples[0];

  switch (typeOf(s)) {
    case 'object': {
      const props = isObj(s.properties) ? s.properties : {};
      const out = {};
      for (const key of Array.isArray(s.required) ? s.required : []) out[key] = valueFor(props[key], root, depth + 1);
      return out;
    }
    case 'array': {
      const n = typeof s.minItems === 'number' && s.minItems > 0 ? Math.floor(s.minItems) : 0;
      const tuple = Array.isArray(s.prefixItems) ? s.prefixItems : Array.isArray(s.items) ? s.items : null;
      const each = isObj(s.items) || s.items === true ? s.items : isObj(s.additionalItems) ? s.additionalItems : undefined;
      const out = [];
      for (let i = 0; i < n; i++) {
        const itemSchema = tuple && i < tuple.length ? tuple[i] : each;
        const v = valueFor(itemSchema, root, depth + 1);
        out.push(s.uniqueItems ? vary(v, i) : v);
      }
      return out;
    }
    case 'integer':
      return numberFor(s, true);
    case 'number':
      return numberFor(s, false);
    case 'boolean':
      return false;
    case 'null':
      return null;
    default:
      return stringFor(s);
  }
}

/** Placeholder args for a tool's required properties, derived from its JSON Schema (`inputSchema`). */
export function argsFromSchema(inputSchema) {
  if (!isObj(inputSchema)) return {};
  const v = valueFor(inputSchema, inputSchema, 0);
  return isObj(v) ? v : {};
}
