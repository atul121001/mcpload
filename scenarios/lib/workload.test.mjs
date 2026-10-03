// Unit tests for the workload profile helpers. Plain Node, no k6 needed:
//   node scenarios/lib/workload.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { normalizeWorkload, pickFlow, pickRows, renderSteps, renderTemplates, templatesIn, workloadThresholds, workloadToolNames } from './workload.js';

// As `mcpload run --workload` writes it (cmd/mcpload/internal/workload).
const WL = {
  name: 'customer-support',
  data: {
    customers: { pick: 'random', rows: [{ id: 'C-1', email: 'a@x.test' }, { id: 'C-2', email: 'b@x.test' }, { id: 'C-3', email: 'c@x.test' }] },
    regions: { pick: 'sequential', rows: ['eu', 'us'] },
  },
  flows: [
    {
      name: 'lookup-orders',
      weight: 60,
      budgets: { p95: 3000, completion: 0.99, steps: { get_orders: { p95: 800 } } },
      steps: [
        { name: 'search_customer', calls: [{ tool: 'search', args: { query: '{{data.customers.email}}' }, as: 'customer', repeat: 1 }] },
        { name: 'get_orders', calls: [{ tool: 'search', args: { query: { $from: 'customer', path: 'results.0.title' } }, repeat: 1 }] },
      ],
    },
    { name: 'check-subscription', weight: 30, budgets: { p95: 2000, p99: 4000, completion: 0.95 }, steps: [{ name: 'fast', calls: [{ tool: 'fast', args: {}, repeat: 1 }] }] },
    { name: 'create-ticket', weight: 10, budgets: { p95: 3000, completion: 0.99 }, steps: [{ name: 'slow', calls: [{ tool: 'slow', args: { note: 'ticket for {{data.customers.id}} in {{data.regions}}' }, repeat: 1 }] }] },
  ],
};

test('normalizeWorkload accepts the CLI output and keeps flow order', () => {
  const wl = normalizeWorkload(WL);
  assert.deepEqual(wl.flows.map((f) => f.name), ['lookup-orders', 'check-subscription', 'create-ticket']);
  assert.deepEqual(workloadToolNames(wl), ['search', 'fast', 'slow']);
  assert.equal(wl.data.regions.pick, 'sequential');
});

test('normalizeWorkload names the flow and step at fault', () => {
  const bad = (mut, re) => {
    const raw = JSON.parse(JSON.stringify(WL));
    mut(raw);
    assert.throws(() => normalizeWorkload(raw), re);
  };
  bad((r) => (r.flows = []), /non-empty "flows"/);
  bad((r) => (r.flows[1].weight = 0), /flow 'check-subscription': weight must be a number > 0/);
  bad((r) => (r.flows[2].name = 'lookup-orders'), /duplicate flow name/);
  bad((r) => (r.flows[0].steps[1].calls[0].args.query.$from = 'nope'), /flow 'lookup-orders': step 2 \(get_orders\) call 1 \(search\) args.query: 'nope' is not/);
  bad((r) => (r.flows[0].steps[0].calls[0].args.query = '{{data.people.email}}'), /names data pool 'people'/);
  bad((r) => (r.flows[0].steps[0].calls[0].args.query = '{{customer.email}}'), /unknown template \{\{customer.email\}\}/);
  bad((r) => (r.flows[0].budgets.steps = { nope: { p95: 1 } }), /budgets.steps names no step 'nope'/);
  bad((r) => (r.flows[0].budgets.completion = 1.5), /completion must be in \(0, 1\]/);
  bad((r) => (r.data.customers.rows = []), /data.customers: needs a non-empty "rows"/);
});

test('pickFlow follows the weights', () => {
  const wl = normalizeWorkload(WL);
  // weights 60/30/10 over [0,1): [0,0.6) lookup, [0.6,0.9) subscription, [0.9,1) ticket
  assert.equal(pickFlow(wl.flows, 0).name, 'lookup-orders');
  assert.equal(pickFlow(wl.flows, 0.5999).name, 'lookup-orders');
  assert.equal(pickFlow(wl.flows, 0.6).name, 'check-subscription');
  assert.equal(pickFlow(wl.flows, 0.8999).name, 'check-subscription');
  assert.equal(pickFlow(wl.flows, 0.9).name, 'create-ticket');
  assert.equal(pickFlow(wl.flows, 0.99999).name, 'create-ticket');
  // over many uniform draws the shares match the weights
  const n = 20000;
  const counts = {};
  for (let i = 0; i < n; i++) {
    const f = pickFlow(wl.flows, (i + 0.5) / n);
    counts[f.name] = (counts[f.name] || 0) + 1;
  }
  assert.deepEqual(counts, { 'lookup-orders': 12000, 'check-subscription': 6000, 'create-ticket': 2000 });
});

test('pickRows: sequential wraps around, random uses rnd', () => {
  const wl = normalizeWorkload(WL);
  assert.deepEqual(pickRows(wl.data, 0, () => 0), { customers: { id: 'C-1', email: 'a@x.test' }, regions: 'eu' });
  assert.deepEqual(pickRows(wl.data, 3, () => 0.99), { customers: { id: 'C-3', email: 'c@x.test' }, regions: 'us' });
  assert.equal(pickRows(wl.data, 4, () => 0.5).regions, 'eu');
});

test('renderTemplates: whole-string templates keep the value, embedded ones become text, references stay', () => {
  const rows = { customers: { id: 'C-7', n: 3 }, regions: 'eu', tags: ['a', 'b'] };
  assert.deepEqual(
    renderTemplates(
      {
        id: '{{data.customers.id}}',
        n: '{{ data.customers.n }}',
        note: 'customer {{data.customers.id}} in {{data.regions}} ({{data.customers.n}} orders)',
        list: ['{{data.regions}}', 'x'],
        all: '{{data.tags}}',
        ref: { $from: 'customer', path: '{{not.a.template}}' },
        plain: 'no templates',
      },
      rows,
    ),
    {
      id: 'C-7',
      n: 3,
      note: 'customer C-7 in eu (3 orders)',
      list: ['eu', 'x'],
      all: ['a', 'b'],
      ref: { $from: 'customer', path: '{{not.a.template}}' },
      plain: 'no templates',
    },
  );
  assert.deepEqual(templatesIn('{{data.a.b}} and {{data.c}}'), [
    { text: '{{data.a.b}}', pool: 'a', column: 'b' },
    { text: '{{data.c}}', pool: 'c', column: undefined },
  ]);
  const wl = normalizeWorkload(WL);
  const steps = renderSteps(wl.flows[2].steps, { customers: { id: 'C-9' }, regions: 'us' });
  assert.equal(steps[0].calls[0].args.note, 'ticket for C-9 in us');
  // the flow itself is untouched (it is shared by every session of the VU)
  assert.equal(wl.flows[2].steps[0].calls[0].args.note, 'ticket for {{data.customers.id}} in {{data.regions}}');
});

test('workloadThresholds: per-flow duration, completion and step budgets', () => {
  assert.deepEqual(workloadThresholds(normalizeWorkload(WL)), {
    'mcp_workload_flow_duration{flow:lookup-orders}': ['p(95)<3000'],
    'mcp_workload_flow_complete{flow:lookup-orders}': ['rate>=0.99'],
    'mcp_workload_step_duration{flow:lookup-orders,flow_step:get_orders}': ['p(95)<800'],
    'mcp_workload_flow_duration{flow:check-subscription}': ['p(95)<2000', 'p(99)<4000'],
    'mcp_workload_flow_complete{flow:check-subscription}': ['rate>=0.95'],
    'mcp_workload_flow_duration{flow:create-ticket}': ['p(95)<3000'],
    'mcp_workload_flow_complete{flow:create-ticket}': ['rate>=0.99'],
  });
});
