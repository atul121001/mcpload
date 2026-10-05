// Unit tests for the resource/prompt mixing helpers. Plain Node, no k6 needed:
//   node scenarios/lib/content.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import {
  contentEnabled,
  entryLabel,
  fillTemplate,
  parallelItem,
  parseContentConfig,
  pickContent,
  planContent,
  promptArgs,
  randomId,
  stepKind,
} from './content.js';

const env = (o) => (name) => o[name];
const seq = (...xs) => {
  let i = 0;
  return () => xs[i++ % xs.length];
};

test('parseContentConfig: defaults are off', () => {
  assert.deepEqual(parseContentConfig(env({})), { resourceRatio: 0, promptRatio: 0 });
  assert.deepEqual(parseContentConfig(env({ RESOURCE_READ_RATIO: '', PROMPT_GET_RATIO: '' })), { resourceRatio: 0, promptRatio: 0 });
  assert.equal(contentEnabled(parseContentConfig(env({}))), false);
  assert.equal(contentEnabled(undefined), false);
});

test('parseContentConfig: values', () => {
  const cc = parseContentConfig(env({ RESOURCE_READ_RATIO: '0.3', PROMPT_GET_RATIO: '0.2' }));
  assert.deepEqual(cc, { resourceRatio: 0.3, promptRatio: 0.2 });
  assert.equal(contentEnabled(cc), true);
  assert.deepEqual(parseContentConfig(env({ RESOURCE_READ_RATIO: '1' })), { resourceRatio: 1, promptRatio: 0 });
  assert.deepEqual(parseContentConfig(env({ RESOURCE_READ_RATIO: '0.7', PROMPT_GET_RATIO: '0.3' })), { resourceRatio: 0.7, promptRatio: 0.3 });
});

test('parseContentConfig: rejects bad values', () => {
  assert.throws(() => parseContentConfig(env({ RESOURCE_READ_RATIO: 'x' })), /RESOURCE_READ_RATIO must be a number/);
  assert.throws(() => parseContentConfig(env({ RESOURCE_READ_RATIO: '-0.1' })), /RESOURCE_READ_RATIO must be between 0 and 1/);
  assert.throws(() => parseContentConfig(env({ PROMPT_GET_RATIO: '1.5' })), /PROMPT_GET_RATIO must be between 0 and 1/);
  assert.throws(() => parseContentConfig(env({ RESOURCE_READ_RATIO: '0.6', PROMPT_GET_RATIO: '0.5' })), /must not exceed 1/);
});

test('randomId: small ids in 1..999', () => {
  assert.equal(randomId(0), '1');
  assert.equal(randomId(0.999999), '999');
  for (let i = 0; i < 200; i++) {
    const n = Number(randomId());
    assert.ok(Number.isInteger(n) && n >= 1 && n <= 999);
  }
});

test('fillTemplate: operators', () => {
  const r = () => 0.5; // -> "500"
  assert.equal(fillTemplate('demo://items/{id}', r), 'demo://items/500');
  assert.equal(fillTemplate('x://a/{a}/b/{b}', seq(0, 0.999999)), 'x://a/1/b/999');
  assert.equal(fillTemplate('x://a{/p,q}', r), 'x://a/500/500');
  assert.equal(fillTemplate('x://a{?q,lang}', r), 'x://a?q=500&lang=500');
  assert.equal(fillTemplate('x://a?x=1{&y}', r), 'x://a?x=1&y=500');
  assert.equal(fillTemplate('x://a{.ext}', r), 'x://a.500');
  assert.equal(fillTemplate('x://a{;v}', r), 'x://a;v=500');
  assert.equal(fillTemplate('x://a{#f}', r), 'x://a#500');
  assert.equal(fillTemplate('x://{+path}', r), 'x://500');
  assert.equal(fillTemplate('x://{name:3}/{list*}', r), 'x://500/500');
  assert.equal(fillTemplate('file:///static.txt', r), 'file:///static.txt');
});

test('planContent: resources, templates and prompts; junk skipped', () => {
  const plan = planContent(
    [{ uri: 'demo://docs/readme', name: 'readme' }, { name: 'no-uri' }, null],
    [{ uriTemplate: 'demo://items/{id}', name: 'item' }, { name: 'x' }],
    [
      { name: 'summarize', arguments: [{ name: 'topic', required: true }, { name: 'style' }] },
      { name: 'plain' },
      { description: 'nameless' },
    ],
  );
  assert.deepEqual(plan, {
    resources: [{ uri: 'demo://docs/readme' }, { uriTemplate: 'demo://items/{id}' }],
    prompts: [
      { name: 'summarize', required: ['topic'] },
      { name: 'plain', required: [] },
    ],
  });
  assert.deepEqual(planContent(undefined, undefined, undefined), { resources: [], prompts: [] });
});

test('promptArgs: required args only, short strings', () => {
  assert.deepEqual(promptArgs({ name: 'p', required: ['topic', 'who'] }, () => 0), { topic: 'test 1', who: 'test 1' });
  assert.deepEqual(promptArgs({ name: 'p', required: [] }), {});
});

test('stepKind: ratios and fallback to tools', () => {
  const cc = { resourceRatio: 0.3, promptRatio: 0.2 };
  const plan = { resources: [{ uri: 'u' }], prompts: [{ name: 'p', required: [] }] };
  assert.equal(stepKind(cc, plan, 0), 'resource');
  assert.equal(stepKind(cc, plan, 0.29), 'resource');
  assert.equal(stepKind(cc, plan, 0.3), 'prompt');
  assert.equal(stepKind(cc, plan, 0.49), 'prompt');
  assert.equal(stepKind(cc, plan, 0.5), 'tool');
  // Nothing listed for a kind: those steps stay tool calls.
  assert.equal(stepKind(cc, { resources: [], prompts: [] }, 0), 'tool');
  assert.equal(stepKind(cc, { resources: [], prompts: [] }, 0.4), 'tool');
  // Off: always tools.
  assert.equal(stepKind({ resourceRatio: 0, promptRatio: 0 }, plan, 0), 'tool');
  assert.equal(stepKind(cc, null, 0), 'tool');
});

test('stepKind: observed shares follow the ratios', () => {
  const cc = { resourceRatio: 0.3, promptRatio: 0.2 };
  const plan = { resources: [{ uri: 'u' }], prompts: [{ name: 'p', required: [] }] };
  const n = 20000;
  const c = { tool: 0, resource: 0, prompt: 0 };
  for (let i = 0; i < n; i++) c[stepKind(cc, plan)]++;
  assert.ok(Math.abs(c.resource / n - 0.3) < 0.03, `resource share ${c.resource / n}`);
  assert.ok(Math.abs(c.prompt / n - 0.2) < 0.03, `prompt share ${c.prompt / n}`);
});

test('pickContent: static resource, template and prompt', () => {
  const plan = {
    resources: [{ uri: 'demo://docs/readme' }, { uriTemplate: 'demo://items/{id}' }],
    prompts: [{ name: 'summarize', required: ['topic'] }],
  };
  assert.deepEqual(pickContent(plan, 'resource', seq(0)), { kind: 'resource', uri: 'demo://docs/readme' });
  assert.deepEqual(pickContent(plan, 'resource', seq(0.9, 0)), { kind: 'resource', uri: 'demo://items/1' });
  assert.deepEqual(pickContent(plan, 'prompt', seq(0.99, 0)), { kind: 'prompt', name: 'summarize', args: { topic: 'test 1' } });
});

test('entryLabel and parallelItem', () => {
  const tool = { name: 'search', args: { query: 'q' }, ownBudget: true };
  const res = { kind: 'resource', uri: 'demo://items/7' };
  const pr = { kind: 'prompt', name: 'summarize', args: { topic: 't' } };
  assert.equal(entryLabel(tool), 'search');
  assert.equal(entryLabel(res), 'resources/read demo://items/7');
  assert.equal(entryLabel(pr), 'prompts/get summarize');
  // Tool items are shaped exactly as before ({name, args} + options).
  assert.deepEqual(parallelItem(tool, { cancelAfterMs: 50 }), { name: 'search', args: { query: 'q' }, cancelAfterMs: 50 });
  assert.deepEqual(parallelItem(tool, undefined), { name: 'search', args: { query: 'q' } });
  assert.deepEqual(parallelItem(res, { meta: { a: 1 } }), { kind: 'resource', uri: 'demo://items/7', meta: { a: 1 } });
  assert.deepEqual(parallelItem(pr), { kind: 'prompt', name: 'summarize', args: { topic: 't' } });
});
