// Unit tests for the long-lived / reconnect-storm helpers. Plain Node, no k6 needed:
//   node scenarios/lib/resilience.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { backoffMs, breakCause, callIdFactory, retryable, sessionAge, staggerS } from './resilience.js';

const err = (type) => ({ type });

test('breakCause', () => {
  const cases = [
    { name: 'no calls', in: [], want: '' },
    { name: 'all ok', in: [null, null], want: '' },
    { name: 'tool errors never break', in: [err('tool_iserror'), err('tool_iserror')], want: '' },
    { name: 'jsonrpc errors never break', in: [err('jsonrpc')], want: '' },
    { name: 'one session_not_found breaks', in: [null, err('session_not_found'), null], want: 'session_not_found' },
    { name: 'session_not_found wins over transport', in: [err('http'), err('session_not_found')], want: 'session_not_found' },
    { name: 'one process_exit breaks (stdio)', in: [null, err('process_exit')], want: 'process_exit' },
    { name: 'process_exit wins over transport', in: [err('timeout'), err('process_exit')], want: 'process_exit' },
    { name: 'all transport errors break', in: [err('http'), err('http')], want: 'http' },
    { name: 'timeouts break', in: [err('timeout')], want: 'timeout' },
    { name: 'one transport error among successes does not', in: [err('http'), null], want: '' },
    { name: 'transport mixed with a tool error does not', in: [err('http'), err('tool_iserror')], want: '' },
  ];
  for (const c of cases) assert.equal(breakCause(c.in), c.want, c.name);
});

test('retryable', () => {
  assert.equal(retryable(null), false);
  assert.equal(retryable(err('tool_iserror')), false);
  assert.equal(retryable(err('jsonrpc')), false);
  for (const t of ['http', 'timeout', 'session_not_found', 'process_exit']) assert.equal(retryable(err(t)), true, t);
});

test('backoffMs', () => {
  const zero = () => 0;
  const one = () => 0.999999;
  const cases = [
    { name: 'first attempt is immediate', a: [1, 100, 5000, 0, zero], want: 0 },
    { name: 'no base: always immediate', a: [5, 0, 5000, 0, zero], want: 0 },
    { name: 'second attempt waits base', a: [2, 100, 5000, 0, zero], want: 100 },
    { name: 'doubles', a: [4, 100, 5000, 0, zero], want: 400 },
    { name: 'capped', a: [20, 100, 5000, 0, zero], want: 5000 },
    { name: 'no cap when max is 0', a: [8, 100, 0, 0, zero], want: 6400 },
    { name: 'full jitter, low draw keeps the delay', a: [3, 100, 5000, 1, zero], want: 200 },
    { name: 'full jitter, high draw removes it', a: [3, 100, 5000, 1, one], want: 0 },
    { name: 'half jitter', a: [3, 100, 5000, 0.5, one], want: 100 },
  ];
  for (const c of cases) assert.equal(backoffMs(...c.a), c.want, c.name);
});

test('callIdFactory', () => {
  const a = callIdFactory('r1', 3);
  const b = callIdFactory('r1', 4);
  assert.deepEqual([a(), a(), b(), a()], ['r1-3-1', 'r1-3-2', 'r1-4-1', 'r1-3-3']);
});

test('sessionAge', () => {
  const cases = [
    [0, 600, 'early'],
    [199, 600, 'early'],
    [200, 600, ''],
    [399, 600, ''],
    [400, 600, 'late'],
    [900, 600, 'late'],
    [10, 0, ''],
  ];
  for (const [e, p, want] of cases) assert.equal(sessionAge(e, p), want, `${e}/${p}`);
});

test('staggerS', () => {
  assert.deepEqual([1, 2, 3, 4].map((vu) => staggerS(vu, 4, 60)), [0, 15, 30, 45]);
  assert.equal(staggerS(5, 4, 60), 0); // wraps (more VUs than planned)
  assert.equal(staggerS(3, 4, 0), 0);
});
