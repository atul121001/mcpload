// Unit tests for the version-skew helpers. Plain Node, no k6 needed:
//   node scenarios/lib/skew.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { classifyFailure, failureReason, isUnknownTool, parseTtlMs, ToolListCache } from './skew.js';

const opts = { failFastMs: 2000, hangMs: 10000 };

test('success and ordinary tool errors are not skew failures', () => {
  assert.equal(classifyFailure(null, 5, opts), null);
  assert.equal(classifyFailure(undefined, 5, opts), null);
  assert.equal(classifyFailure({ type: 'tool_iserror', status: 200, message: 'flaky: simulated tool failure' }, 3, opts), null);
});

test('classification table', () => {
  const cases = [
    // [name, err, ms, kind, reason]
    ['new build rejects the old protocol', { type: 'jsonrpc', status: 400, code: -32022, message: 'Unsupported protocol version' }, 4, 'fast', 'unsupported_protocol'],
    ['old build: no session header', { type: 'http', status: 400, code: -32000, message: 'Bad Request: no Mcp-Session-Id header' }, 3, 'fast', 'http_400'],
    ['old build: unknown session', { type: 'session_not_found', status: 404, code: -32001, message: 'Session not found' }, 2, 'fast', 'session_not_found'],
    ['unknown tool as isError (TS SDK)', { type: 'tool_iserror', status: 200, message: 'MCP error -32602: Tool new_tool not found' }, 2, 'fast', 'unknown_tool'],
    ['unknown tool as JSON-RPC error (Go SDK)', { type: 'jsonrpc', status: 200, code: -32602, message: 'unknown tool "new_tool"' }, 2, 'fast', 'unknown_tool'],
    ['method not found', { type: 'jsonrpc', status: 200, code: -32601, message: 'Method not found' }, 2, 'fast', 'method_not_found'],
    ['other JSON-RPC error on 200', { type: 'jsonrpc', status: 200, code: -32603, message: 'Internal error' }, 2, 'fast', 'jsonrpc_error'],
    ['5xx', { type: 'http', status: 502, message: 'Bad Gateway' }, 20, 'fast', 'http_502'],
    ['header mismatch', { type: 'header_mismatch', status: 400, code: -32020, message: 'mismatch' }, 2, 'fast', 'header_mismatch'],
    ['timeout is a hang however short', { type: 'timeout', message: 'context deadline exceeded' }, 500, 'hang', 'timeout'],
    ['typed error after HANG_MS is a hang', { type: 'jsonrpc', status: 400, code: -32022, message: 'x' }, 12000, 'hang', 'unsupported_protocol'],
    ['typed error after FAIL_FAST_MS is slow', { type: 'http', status: 400, message: 'x' }, 2500, 'slow', 'http_400'],
    ['untyped transport error is slow, not fast', { type: 'http', message: 'connection reset by peer' }, 3, 'slow', 'transport'],
  ];
  for (const [name, err, ms, kind, reason] of cases) {
    const c = classifyFailure(err, ms, opts);
    assert.ok(c, name);
    assert.equal(c.kind, kind, `${name}: kind`);
    assert.equal(c.reason, reason, `${name}: reason`);
  }
});

test('defaults: FAIL_FAST_MS 2000, HANG_MS 10000', () => {
  const e = { type: 'http', status: 400, message: 'x' };
  assert.equal(classifyFailure(e, 1999).kind, 'fast');
  assert.equal(classifyFailure(e, 2000).kind, 'slow');
  assert.equal(classifyFailure(e, 10000).kind, 'hang');
});

test('failureReason and isUnknownTool', () => {
  assert.equal(failureReason(null), '');
  assert.equal(failureReason({ type: 'auth', status: 401 }), 'auth');
  assert.equal(isUnknownTool({ type: 'tool_iserror', message: 'Tool foo not found' }), true);
  assert.equal(isUnknownTool({ type: 'tool_iserror', message: 'search backend not found' }), false);
  // an HTTP 404 page is not an unknown tool
  assert.equal(isUnknownTool({ type: 'http', status: 404, message: 'tool not found' }), false);
});

test('parseTtlMs', () => {
  for (const off of [undefined, '', '0', 'off', 'false']) assert.equal(parseTtlMs(off), 0, String(off));
  assert.equal(parseTtlMs('5m'), 300000);
  assert.equal(parseTtlMs('30s'), 30000);
  assert.equal(parseTtlMs('1h'), 3600000);
  assert.equal(parseTtlMs('500ms'), 500);
  assert.equal(parseTtlMs('1500'), 1500);
  assert.throws(() => parseTtlMs('soon'), /TOOLS_CACHE_TTL/);
});

test('ToolListCache keeps a list for its TTL only', () => {
  const c = new ToolListCache(1000);
  assert.equal(c.get(0), null);
  c.put([{ name: 'new_tool' }], 100, 'skew-new');
  assert.deepEqual(c.get(1099), [{ name: 'new_tool' }]);
  assert.equal(c.from, 'skew-new');
  assert.equal(c.get(1100), null);
  const off = new ToolListCache(0);
  off.put([{ name: 'x' }], 0);
  assert.equal(off.get(1), null);
});
