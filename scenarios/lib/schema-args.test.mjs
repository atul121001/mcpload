// Unit tests for the pure scenario helpers. Plain Node, no k6 needed:
//   node scenarios/lib/schema-args.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { argsFromSchema } from './schema-args.js';
import { planTools, thresholdToolNames, pick, withoutTools, DEMO_TOOLS } from './tools.js';
import { stepAt, stepLevels, stepSchedule } from './steps.js';

// config.js reads k6's __ENV at import time.
globalThis.__ENV = {};
const { arrivalRate, durationSeconds } = await import('./config.js');

const obj = (properties, required) => ({ type: 'object', properties, required: required || Object.keys(properties) });

test('only required properties, basic types', () => {
  assert.deepEqual(
    argsFromSchema(obj({ s: { type: 'string' }, i: { type: 'integer' }, n: { type: 'number' }, b: { type: 'boolean' }, opt: { type: 'string' } }, ['s', 'i', 'n', 'b'])),
    { s: 'load-test', i: 1, n: 1, b: false },
  );
  assert.deepEqual(argsFromSchema(undefined), {});
  assert.deepEqual(argsFromSchema({ type: 'object' }), {});
});

test('const, default, enum, examples (in that order)', () => {
  assert.deepEqual(
    argsFromSchema(
      obj({
        c: { const: 'fixed', default: 'd' },
        d: { type: 'integer', default: 7, enum: [1, 7] },
        e: { type: 'string', enum: ['celsius', 'fahrenheit'] },
        x: { type: 'string', examples: ['Paris', 'Rome'] },
      }),
    ),
    { c: 'fixed', d: 7, e: 'celsius', x: 'Paris' },
  );
});

test('nested objects recurse into required properties only', () => {
  const s = obj({ loc: obj({ city: { type: 'string' }, geo: obj({ lat: { type: 'number', minimum: -90 } }), note: { type: 'string' } }, ['city', 'geo']) });
  assert.deepEqual(argsFromSchema(s), { loc: { city: 'load-test', geo: { lat: -90 } } });
});

test('arrays honour minItems and items / tuple items / uniqueItems', () => {
  const s = obj({
    none: { type: 'array', items: { type: 'string' } },
    two: { type: 'array', minItems: 2, items: obj({ id: { type: 'integer', minimum: 10 } }) },
    tuple: { type: 'array', minItems: 2, prefixItems: [{ type: 'string', format: 'email' }, { type: 'boolean' }] },
    uniq: { type: 'array', minItems: 3, uniqueItems: true, items: { type: 'string' } },
  });
  assert.deepEqual(argsFromSchema(s), {
    none: [],
    two: [{ id: 10 }, { id: 10 }],
    tuple: ['load-test@example.com', false],
    uniq: ['load-test', 'load-test-1', 'load-test-2'],
  });
});

test('anyOf / oneOf pick the first non-null branch; type arrays skip null', () => {
  const s = obj({
    a: { anyOf: [{ type: 'null' }, { type: 'integer', minimum: 3 }] },
    o: { oneOf: [{ type: 'string', format: 'uuid' }, { type: 'number' }] },
    t: { type: ['null', 'boolean'] },
  });
  assert.deepEqual(argsFromSchema(s), { a: 3, o: '00000000-0000-4000-8000-000000000000', t: false });
});

test('string formats and lengths', () => {
  const s = obj({
    u: { type: 'string', format: 'uri' },
    e: { type: 'string', format: 'email' },
    id: { type: 'string', format: 'uuid' },
    d: { type: 'string', format: 'date' },
    dt: { type: 'string', format: 'date-time' },
    long: { type: 'string', minLength: 12 },
    short: { type: 'string', maxLength: 4 },
  });
  const a = argsFromSchema(s);
  assert.match(a.u, /^https:\/\//);
  assert.match(a.e, /^[^@]+@[^@]+\.[a-z]+$/);
  assert.match(a.id, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  assert.equal(a.d, '2026-01-01');
  assert.ok(!Number.isNaN(Date.parse(a.dt)));
  assert.equal(a.long.length, 12);
  assert.equal(a.short.length, 4);
});

test('numbers: minimum, exclusiveMinimum, maximum, multipleOf', () => {
  const s = obj({
    min: { type: 'integer', minimum: 5 },
    exInt: { type: 'integer', exclusiveMinimum: 0 },
    exInt5: { type: 'integer', exclusiveMinimum: 5 },
    exNum: { type: 'number', exclusiveMinimum: 0, exclusiveMaximum: 0.5 },
    mult: { type: 'integer', minimum: 5, multipleOf: 3 },
    multF: { type: 'number', minimum: 0.1, multipleOf: 0.25 },
    max: { type: 'number', maximum: 0 },
    draft4: { type: 'integer', minimum: 2, exclusiveMinimum: true },
    range: { type: 'integer', minimum: 100, maximum: 200, multipleOf: 50 },
  });
  const a = argsFromSchema(s);
  assert.equal(a.min, 5);
  assert.equal(a.exInt, 1);
  assert.equal(a.exInt5, 6);
  assert.ok(a.exNum > 0 && a.exNum < 0.5, `exNum ${a.exNum}`);
  assert.equal(a.mult, 6);
  assert.equal(a.multF, 0.25);
  assert.equal(a.max, 0);
  assert.equal(a.draft4, 3);
  assert.equal(a.range, 100);
});

test('local $ref: #/$defs and #/definitions, with cycles bounded', () => {
  const s = {
    type: 'object',
    $defs: { Loc: obj({ city: { $ref: '#/definitions/City' } }) },
    definitions: { City: { type: 'string', minLength: 3, maxLength: 3 }, Node: obj({ next: { $ref: '#/definitions/Node' } }) },
    properties: { loc: { $ref: '#/$defs/Loc' }, n: { $ref: '#/definitions/Node' }, missing: { $ref: '#/$defs/Nope' } },
    required: ['loc', 'n', 'missing'],
  };
  const a = argsFromSchema(s);
  assert.deepEqual(a.loc, { city: 'loa' });
  assert.equal(typeof a.n, 'object');
  assert.equal(a.missing, 'load-test');
});

test('allOf merges required properties', () => {
  const s = { allOf: [obj({ a: { type: 'string' } }), obj({ b: { type: 'integer', minimum: 2 } })] };
  assert.deepEqual(argsFromSchema(s), { a: 'load-test', b: 2 });
});

const demoListed = DEMO_TOOLS.map((name) => ({ name, inputSchema: name === 'search' ? obj({ query: { type: 'string' } }) : { type: 'object' } }));
const realListed = [
  { name: 'get_weather', inputSchema: obj({ city: { type: 'string' } }) },
  { name: 'search', inputSchema: obj({ q: { type: 'string', minLength: 2 } }) },
];

test('demo server + unset TOOL_MIX: demo mix, demo args, own budgets', () => {
  const tt = planTools(demoListed, { toolMix: null, toolArgs: {}, userBudgets: {} });
  assert.equal(tt.demo, true);
  assert.deepEqual(tt.table.map((e) => e.name).sort(), [...DEMO_TOOLS].sort());
  assert.equal(tt.total, 11);
  assert.deepEqual(tt.table.find((e) => e.name === 'search').args, { query: 'invoices', limit: 5 });
  assert.ok(tt.table.every((e) => e.ownBudget));
});

test('real server listing `search`: no demo hijack, uniform, schema args, catch-all budget', () => {
  const tt = planTools(realListed, { toolMix: null, toolArgs: {}, userBudgets: {} });
  assert.equal(tt.demo, false);
  assert.deepEqual(tt.table.map((e) => [e.name, e.cum]), [['get_weather', 1], ['search', 2]]);
  assert.deepEqual(tt.table[1].args, { q: 'load-test' });
  assert.ok(tt.table.every((e) => !e.ownBudget));
  // TOOL_ARGS and TOOL_BUDGETS overrides
  const tt2 = planTools(realListed, { toolMix: null, toolArgs: { search: { q: 'x' } }, userBudgets: { get_weather: { p95: 2000 } } });
  assert.deepEqual(tt2.table[1].args, { q: 'x' });
  assert.equal(tt2.table[0].ownBudget, true);
  assert.equal(tt2.table[1].ownBudget, false);
});

test('explicit TOOL_MIX: unknown names reported, typo-only mix yields empty table', () => {
  const tt = planTools(realListed, { toolMix: { get_weather: 2, get_wether: 1, search: 0 }, toolArgs: {}, userBudgets: {} });
  assert.deepEqual(tt.table.map((e) => e.name), ['get_weather']);
  assert.deepEqual(tt.unknown, ['get_wether']);
  assert.equal(tt.table[0].ownBudget, true);
  const typo = planTools(demoListed, { toolMix: { serch: 1 }, toolArgs: {}, userBudgets: {} });
  assert.equal(typo.table.length, 0);
  assert.deepEqual(typo.unknown, ['serch']);
  assert.deepEqual(typo.listedNames, DEMO_TOOLS);
});

test('thresholdToolNames and pick', () => {
  assert.deepEqual(thresholdToolNames({ toolMix: null, toolBudgets: { flaky: {} } }), DEMO_TOOLS);
  assert.deepEqual(thresholdToolNames({ toolMix: { a: 1, b: 0 }, toolBudgets: { flaky: {}, c: {} } }), ['a', 'flaky', 'c']);
  const tt = planTools(realListed, { toolMix: { get_weather: 3, search: 1 }, toolArgs: {}, userBudgets: {} });
  assert.equal(pick(tt, 0).name, 'get_weather');
  assert.equal(pick(tt, 0.74).name, 'get_weather');
  assert.equal(pick(tt, 0.76).name, 'search');
});

test('arrivalRate: fractional RATE becomes an integer rate per larger time unit', () => {
  assert.deepEqual(arrivalRate(2, '1s'), { rate: 2, timeUnit: '1s', perSecond: 2 });
  assert.deepEqual(arrivalRate(0.05, '1s'), { rate: 3, timeUnit: '1m', perSecond: 0.05 });
  assert.equal(arrivalRate(0.5, '1s').rate, 30);
  assert.equal(arrivalRate(0.5, '1s').timeUnit, '1m');
  assert.deepEqual(arrivalRate(3, '1m'), { rate: 3, timeUnit: '1m', perSecond: 0.05 });
  assert.deepEqual(arrivalRate(1, '1h'), { rate: 1, timeUnit: '1h', perSecond: 1 / 3600 });
  assert.equal(arrivalRate(10, '10s').rate, 1);
  assert.equal(arrivalRate(1 / 7, '1s').timeUnit, '1h');
  assert.equal(durationSeconds('500ms'), 0.5);
  assert.throws(() => arrivalRate(0, '1s'));
  assert.throws(() => durationSeconds('soon'));
});

test('withoutTools drops tools and keeps the other weights', () => {
  const listed = ['a', 'b', 'c'].map((name) => ({ name, inputSchema: { type: 'object' } }));
  const tt = planTools(listed, { toolMix: { a: 3, b: 1, c: 2 }, toolArgs: {} });
  const w = withoutTools(tt, ['b']);
  assert.deepEqual(w.table.map((e) => [e.name, e.cum]), [['a', 3], ['c', 5]]);
  assert.equal(w.total, 5);
  assert.equal(tt.total, 6); // input untouched
  assert.equal(pick(w, 0.7).name, 'c');
  assert.equal(withoutTools(tt, ['a', 'b', 'c']).table.length, 0);
});

test('stepLevels: STEPS list or geometric START/STEP_FACTOR/MAX_VUS', () => {
  assert.deepEqual(stepLevels({ steps: '10, 25,50' }), [10, 25, 50]);
  assert.deepEqual(stepLevels({ start: 5, factor: 2, max: 40 }), [5, 10, 20, 40]);
  assert.deepEqual(stepLevels({ start: 10, factor: 1.5, max: 40 }), [10, 15, 23, 34]);
  assert.deepEqual(stepLevels({ steps: '', start: 3, factor: 3, max: 30 }), [3, 9, 27]);
  assert.throws(() => stepLevels({ steps: '10,5' }), /increase/);
  assert.throws(() => stepLevels({ steps: '10,x' }), /positive integers/);
  assert.throws(() => stepLevels({ start: 10, factor: 1, max: 40 }), /STEP_FACTOR/);
});

test('stepSchedule and stepAt: ramps are unmeasured, holds are tagged', () => {
  const s = stepSchedule([5, 10], 20, 5);
  assert.deepEqual(s.stages, [
    { duration: '5s', target: 5 },
    { duration: '20s', target: 5 },
    { duration: '5s', target: 10 },
    { duration: '20s', target: 10 },
  ]);
  assert.deepEqual(s.windows, [{ vus: 5, from: 5, to: 25 }, { vus: 10, from: 30, to: 50 }]);
  assert.equal(s.totalS, 50);
  assert.equal(stepAt(s.windows, 2), null);
  assert.equal(stepAt(s.windows, 5).vus, 5);
  assert.equal(stepAt(s.windows, 27), null);
  assert.equal(stepAt(s.windows, 49.9).vus, 10);
  assert.equal(stepAt(s.windows, 50), null);
});
