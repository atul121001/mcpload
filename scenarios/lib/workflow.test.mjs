// Unit tests for the agent-workflow plan helpers. Plain Node, no k6 needed:
//   node scenarios/lib/workflow.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import {
  DEFAULT_WORKFLOW,
  expandStep,
  getPath,
  missingTools,
  newScope,
  normalizeWorkflow,
  planToolNames,
  recordResults,
  resolveRef,
  resultView,
  thinkSecondsFor,
  workflowThresholds,
} from './workflow.js';
import { DEMO_TOOLS } from './tools.js';

const text = (t, extra) => Object.assign({ isError: false, content: [{ type: 'text', text: t }] }, extra || {});
const searchResult = (query, n) =>
  text(JSON.stringify({ query, results: Array.from({ length: n }, (_, i) => ({ title: `Result ${i + 1} for "${query}"`, url: `https://example.com/search/${encodeURIComponent(query)}/${i + 1}` })) }));

test('default plan is valid and only uses the demo tools', () => {
  const plan = normalizeWorkflow(DEFAULT_WORKFLOW);
  assert.deepEqual(plan.steps.map((s) => s.name), ['gather', 'inspect', 'act', 'report']);
  assert.ok(planToolNames(plan).every((n) => DEMO_TOOLS.indexOf(n) >= 0));
  assert.deepEqual(missingTools(plan, DEMO_TOOLS), []);
  assert.deepEqual(missingTools(plan, ['search', 'fast']), ['slow', 'big', 'flaky']);
});

test('normalizeWorkflow: bare steps array, defaults, and validation errors', () => {
  const plan = normalizeWorkflow([{ calls: [{ tool: 'a', repeat: 2 }] }, { calls: [{ tool: 'b' }], thinkMs: 0 }]);
  assert.deepEqual(plan, {
    steps: [
      { name: 'step1', calls: [{ tool: 'a', args: {}, repeat: 2 }] },
      { name: 'step2', thinkMs: 0, calls: [{ tool: 'b', args: {}, repeat: 1 }] },
    ],
  });
  const bad = (raw, re) => assert.throws(() => normalizeWorkflow(raw), re);
  bad({}, /non-empty "steps"/);
  bad([{ calls: [] }], /non-empty "calls"/);
  bad([{ calls: [{ tool: 'a', bogus: 1 }] }], /unknown key 'bogus'/);
  bad([{ name: 'a b', calls: [{ tool: 'a' }] }], /name must match/);
  bad([{ name: 'x', calls: [{ tool: 'a' }] }, { name: 'x', calls: [{ tool: 'a' }] }], /duplicate step name/);
  bad([{ calls: [{ tool: 'a', repeat: 0 }] }], /repeat must be an integer/);
  // a reference must name an `as` of an EARLIER step (calls within one step run in parallel)
  bad([{ calls: [{ tool: 'a', as: 'x' }, { tool: 'b', args: { q: { $from: 'x' } } }] }], /not the 'as' name of a call in an earlier step/);
  bad([{ calls: [{ tool: 'a', args: { q: { $from: 'nope' } } }] }], /'nope' is not/);
  bad([{ calls: [{ tool: 'a', args: { q: { $from: '$item' } } }] }], /only available inside a call with forEach/);
  bad([{ calls: [{ tool: 'a', as: 'x' }] }, { calls: [{ tool: 'b', as: 'x' }] }], /duplicate as name/);
  bad([{ calls: [{ tool: 'a', as: 'x' }] }, { calls: [{ tool: 'b', repeat: 2, forEach: { $from: 'x' } }] }], /either repeat or forEach/);
  bad([{ calls: [{ tool: 'a', as: 'x' }] }, { calls: [{ tool: 'b', args: { q: { $from: 'x', match: '(' } } }] }], /not a valid regex/);
  // forEach.max defaults
  const fe = normalizeWorkflow([{ calls: [{ tool: 'a', as: 'x' }] }, { calls: [{ tool: 'b', forEach: { $from: 'x', path: 'r' } }] }]);
  assert.equal(fe.steps[1].calls[0].forEach.max, 5);
});

test('resultView: structuredContent, then JSON text, then text', () => {
  assert.deepEqual(resultView(text('{"a":1}', { structuredContent: { b: 2 } })), { b: 2 });
  assert.deepEqual(resultView(text('{"a":1}')), { a: 1 });
  assert.equal(resultView(text('slept 300ms')), 'slept 300ms');
  assert.deepEqual(resultView({ content: [{ type: 'image', data: 'x' }] }), [{ type: 'image', data: 'x' }]);
});

test('getPath and resolveRef: paths, brackets, regex match, failed vs missing', () => {
  const v = { results: [{ id: 'A-17', url: 'https://x/items/42' }] };
  assert.equal(getPath(v, 'results.0.id'), 'A-17');
  assert.equal(getPath(v, 'results[0].id'), 'A-17');
  assert.equal(getPath(v, ''), v);
  assert.equal(getPath(v, 'results.3.id'), undefined);
  assert.equal(getPath('str', 'length'), undefined);

  const scope = newScope();
  scope.values.hits = v;
  scope.values.msg = 'order id=991 created';
  scope.failed.broken = true;
  assert.deepEqual(resolveRef({ $from: 'hits', path: 'results.0.url', match: '/items/(\\d+)$' }, scope), { ok: true, value: '42' });
  assert.deepEqual(resolveRef({ $from: 'msg', match: 'id=\\d+' }, scope), { ok: true, value: 'id=991' });
  assert.equal(resolveRef({ $from: 'hits', path: 'nope' }, scope).reason, 'missing');
  assert.equal(resolveRef({ $from: 'msg', match: 'zzz' }, scope).reason, 'missing');
  assert.equal(resolveRef({ $from: 'broken' }, scope).reason, 'failed');
});

test('default plan runs end to end on demo-shaped results', () => {
  const plan = normalizeWorkflow(DEFAULT_WORKFLOW);
  const scope = newScope();
  const base = (name) => (name === 'search' ? { query: 'base', limit: 5 } : {});

  // step 1: gather
  let ex = expandStep(plan.steps[0], scope, base);
  assert.equal(ex.hasRefs, false);
  assert.deepEqual(ex.calls.map((c) => [c.name, c.args]), [
    ['search', { query: 'overdue invoices', limit: 5 }],
    ['search', { query: 'customer accounts', limit: 3 }],
    ['fast', {}],
  ]);
  recordResults(plan.steps[0], ex.calls, [searchResult('overdue invoices', 5), searchResult('customer accounts', 3), text('ok')], scope);

  // step 2: one search per top-3 invoice hit (data-dependent fan-out) next to a slow call
  ex = expandStep(plan.steps[1], scope, base);
  assert.equal(ex.hasRefs, true);
  assert.deepEqual(ex.problems, []);
  assert.deepEqual(ex.calls.map((c) => c.name), ['search', 'search', 'search', 'slow']);
  assert.equal(ex.calls[1].args.query, 'Result 2 for "overdue invoices"');
  assert.equal(ex.calls[1].args.limit, 2);
  assert.deepEqual(ex.calls[3].args, { ms: 200 });
  // one of the three fails: `details` keeps the two that succeeded
  recordResults(plan.steps[1], ex.calls, [{ isError: true, content: [], error: { type: 'tool_iserror' } }, searchResult(ex.calls[1].args.query, 2), searchResult(ex.calls[2].args.query, 2), text('slept 200ms')], scope);
  assert.equal(scope.values.details.length, 2);

  // step 3: query built from step 2's first successful result
  ex = expandStep(plan.steps[2], scope, base);
  assert.deepEqual(ex.problems, []);
  assert.equal(ex.calls[0].args.query, 'Result 1 for "Result 2 for "overdue invoices""');
  recordResults(plan.steps[2], ex.calls, [searchResult('Invoice 7781', 1), text('x'), text('flaky: ok')], scope);

  // step 4: an id pulled out of step 3's result url with a regex
  ex = expandStep(plan.steps[3], scope, base);
  assert.deepEqual(ex.calls, [{ name: 'search', args: { query: 'Invoice%207781', limit: 1 }, call: 0 }]);
});

test('expandStep: repeat, forEach max, failed source vs missing path', () => {
  const plan = normalizeWorkflow([
    { calls: [{ tool: 'list', as: 'l' }, { tool: 'ping', repeat: 3, as: 'p' }] },
    { calls: [{ tool: 'get', forEach: { $from: 'l', path: 'ids', max: 2 }, args: { id: { $from: '$item' } } }] },
    { calls: [{ tool: 'use', args: { first: { $from: 'p', path: '0' } } }] },
  ]);
  const scope = newScope();
  let ex = expandStep(plan.steps[0], scope);
  assert.equal(ex.calls.length, 4);
  recordResults(plan.steps[0], ex.calls, [text('{"ids":[1,2,3]}'), text('a'), { isError: true }, text('c')], scope);
  assert.deepEqual(scope.values.p, ['a', 'c']);

  ex = expandStep(plan.steps[1], scope);
  assert.deepEqual(ex.calls.map((c) => c.args), [{ id: 1 }, { id: 2 }]);

  // the source call failed: not a plan problem
  const s2 = newScope();
  recordResults(plan.steps[0], expandStep(plan.steps[0], s2).calls, [{ error: { type: 'timeout' } }, text('a'), text('b'), text('c')], s2);
  ex = expandStep(plan.steps[1], s2);
  assert.deepEqual(ex.problems.map((p) => p.reason), ['failed']);
  assert.equal(ex.calls.length, 0);

  // the source succeeded but has no such path: the plan does not fit the server
  const s3 = newScope();
  recordResults(plan.steps[0], expandStep(plan.steps[0], s3).calls, [text('{"items":[]}'), text('a'), text('b'), text('c')], s3);
  ex = expandStep(plan.steps[1], s3);
  assert.deepEqual(ex.problems.map((p) => p.reason), ['missing']);
  assert.match(ex.problems[0].detail, /l\.ids not found/);
});

test('workflowThresholds: workflow, completion and per-step budgets', () => {
  const plan = normalizeWorkflow([{ name: 'a', calls: [{ tool: 'x' }] }, { name: 'b', calls: [{ tool: 'y' }] }]);
  assert.deepEqual(workflowThresholds(plan, { workflowP95: 5000, stepP95: 1000, stepBudgets: { b: { p95: 300, p99: 900 } }, minComplete: 0.95 }), {
    mcp_workflow_duration: ['p(95)<5000'],
    mcp_workflow_complete: ['rate>=0.95'],
    'mcp_workflow_step_duration{step:a}': ['p(95)<1000'],
    'mcp_workflow_step_duration{step:b}': ['p(95)<300', 'p(99)<900'],
  });
  const t = workflowThresholds(plan, { workflowP95: 1, workflowP99: 2, stepP95: 3, stepP99: 4, minComplete: 1 });
  assert.deepEqual(t.mcp_workflow_duration, ['p(95)<1', 'p(99)<2']);
  assert.deepEqual(t['mcp_workflow_step_duration{step:a}'], ['p(95)<3', 'p(99)<4']);
});

test('thinkSecondsFor: exponential, capped at 5x, 0 when disabled', () => {
  assert.equal(thinkSecondsFor(0, 0.5), 0);
  assert.ok(thinkSecondsFor(500, 0) === 0);
  assert.ok(Math.abs(thinkSecondsFor(500, 1 - Math.exp(-1)) - 0.5) < 1e-9);
  assert.equal(thinkSecondsFor(500, 0.999999999), 2.5);
});
