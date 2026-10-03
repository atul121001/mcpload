// Unit tests for the cancellation helpers. Plain Node, no k6 needed:
//   node scenarios/lib/cancel.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { cancelOptions, parseCancelConfig } from './cancel.js';

const env = (o) => (name) => o[name];

test('parseCancelConfig: defaults (never cancel)', () => {
  assert.deepEqual(parseCancelConfig(env({})), { rate: 0, afterMs: 150, tools: null });
  assert.deepEqual(parseCancelConfig(env({ CANCEL_RATE: '', CANCEL_TOOLS: ' ' })), { rate: 0, afterMs: 150, tools: null });
});

test('parseCancelConfig: values and tool list', () => {
  assert.deepEqual(parseCancelConfig(env({ CANCEL_RATE: '0.2', CANCEL_AFTER_MS: '75', CANCEL_TOOLS: 'slow, big,' })), {
    rate: 0.2,
    afterMs: 75,
    tools: ['slow', 'big'],
  });
});

test('parseCancelConfig: rejects bad values', () => {
  assert.throws(() => parseCancelConfig(env({ CANCEL_RATE: '2' })), /CANCEL_RATE/);
  assert.throws(() => parseCancelConfig(env({ CANCEL_RATE: 'x' })), /CANCEL_RATE/);
  assert.throws(() => parseCancelConfig(env({ CANCEL_AFTER_MS: '0' })), /CANCEL_AFTER_MS/);
});

test('cancelOptions: rate, eligibility and rnd', () => {
  const cc = { rate: 0.25, afterMs: 150, tools: ['slow'] };
  assert.deepEqual(cancelOptions('slow', cc, 0.1), { cancelAfterMs: 150 });
  assert.equal(cancelOptions('slow', cc, 0.25), undefined);
  assert.equal(cancelOptions('fast', cc, 0), undefined);
  assert.deepEqual(cancelOptions('fast', { rate: 1, afterMs: 50, tools: null }, 0.99), { cancelAfterMs: 50 });
  assert.equal(cancelOptions('slow', { rate: 0, afterMs: 150, tools: null }, 0), undefined);
  assert.equal(cancelOptions('slow', undefined, 0), undefined);
});
