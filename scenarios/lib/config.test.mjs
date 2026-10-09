// Unit tests for the target options in config.js (MCP_URL / MCP_COMMAND). Plain Node, no k6 needed:
//   node scenarios/lib/config.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';

// config.js reads k6's __ENV at import time; each load() imports a fresh copy with the given env.
let n = 0;
async function load(env) {
  globalThis.__ENV = env;
  return import(`./config.js?case=${n++}`);
}

const getter = (vars) => (name) => (vars[name] === undefined || vars[name] === '' ? undefined : vars[name]);

test('parseCommandConfig: unset -> undefined (HTTP)', async () => {
  const { parseCommandConfig } = await load({});
  assert.equal(parseCommandConfig(getter({})), undefined);
  assert.equal(parseCommandConfig(getter({ MCP_COMMAND: '' })), undefined);
});

test('parseCommandConfig: program + args, env, cwd', async () => {
  const { parseCommandConfig } = await load({});
  assert.deepEqual(parseCommandConfig(getter({ MCP_COMMAND: '["node","server.mjs","--stdio"]' })), {
    command: 'node',
    args: ['server.mjs', '--stdio'],
  });
  assert.deepEqual(parseCommandConfig(getter({ MCP_COMMAND: '["./server"]' })), { command: './server', args: [] });
  assert.deepEqual(
    parseCommandConfig(
      getter({ MCP_COMMAND: '["uvx","my-server"]', MCP_COMMAND_ENV: '{"API_KEY":"x","MODE":"load"}', MCP_COMMAND_CWD: '/srv' }),
    ),
    { command: 'uvx', args: ['my-server'], env: { API_KEY: 'x', MODE: 'load' }, cwd: '/srv' },
  );
  // An empty env object adds nothing.
  assert.deepEqual(parseCommandConfig(getter({ MCP_COMMAND: '["a"]', MCP_COMMAND_ENV: '{}' })), { command: 'a', args: [] });
});

test('parseCommandConfig: invalid values', async () => {
  const { parseCommandConfig } = await load({});
  const bad = (vars, re) => assert.throws(() => parseCommandConfig(getter(vars)), re);
  bad({ MCP_COMMAND: 'node server.mjs' }, /MCP_COMMAND must be a JSON array/);
  bad({ MCP_COMMAND: '[]' }, /non-empty JSON array/);
  bad({ MCP_COMMAND: '[""]' }, /non-empty JSON array/);
  bad({ MCP_COMMAND: '"node"' }, /non-empty JSON array/);
  bad({ MCP_COMMAND: '["node",1]' }, /non-empty JSON array/);
  bad({ MCP_COMMAND: '["node"]', MCP_COMMAND_ENV: '[]' }, /MCP_COMMAND_ENV must be a JSON object/);
  bad({ MCP_COMMAND: '["node"]', MCP_COMMAND_ENV: '{"N":1}' }, /value for 'N' must be a string/);
  // A JSON syntax error must not echo the (possibly secret) value.
  assert.throws(
    () => parseCommandConfig(getter({ MCP_COMMAND: '["node"]', MCP_COMMAND_ENV: '{"TOKEN":"s3cret"' })),
    (e) => /MCP_COMMAND_ENV/.test(e.message) && !/s3cret/.test(e.message),
  );
});

test('describeTarget: URL, or the command line without env values', async () => {
  const { describeTarget } = await load({});
  assert.equal(describeTarget('http://h/mcp', undefined), 'http://h/mcp');
  assert.equal(
    describeTarget('http://h/mcp', { command: 'node', args: ['my server.mjs', '--stdio'], env: { TOKEN: 's3cret' } }),
    'stdio: node "my server.mjs" --stdio',
  );
  assert.equal(describeTarget('', { command: 'npx', args: ['-y', 'pkg'], cwd: '/srv' }), 'stdio: npx -y pkg (cwd /srv)');
});

test('clientOptions: HTTP target (MCP_URL, headers, auth)', async () => {
  const { clientOptions, config } = await load({ MCP_URL: 'http://h:1/mcp', MCP_TOKEN: 't', MCP_HEADERS: '{"X-A":"1"}' });
  assert.equal(config.transport, 'http');
  assert.equal(config.target, 'http://h:1/mcp');
  const o = clientOptions();
  assert.equal(o.url, 'http://h:1/mcp');
  assert.deepEqual(o.headers, { 'X-A': '1' });
  assert.deepEqual(o.auth, { type: 'bearer', token: 't' });
  assert.equal(o.command, undefined);
  assert.equal(o.protocol, 'auto');
});

test('clientOptions: stdio target (MCP_COMMAND wins over MCP_URL; no url, headers or auth)', async () => {
  const { clientOptions, config } = await load({
    MCP_URL: 'http://h:1/mcp',
    MCP_TOKEN: 't',
    MCP_COMMAND: '["node","server.mjs","--stdio"]',
    MCP_COMMAND_ENV: '{"PERSONA":"leaky"}',
    MCP_COMMAND_CWD: 'demo',
    MCP_TIMEOUT: '5s',
  });
  assert.equal(config.transport, 'stdio');
  assert.equal(config.target, 'stdio: node server.mjs --stdio (cwd demo)');
  const o = clientOptions({ includePayloads: true });
  assert.equal(o.command, 'node');
  assert.deepEqual(o.args, ['server.mjs', '--stdio']);
  assert.deepEqual(o.env, { PERSONA: 'leaky' });
  assert.equal(o.cwd, 'demo');
  assert.equal(o.timeout, '5s');
  assert.equal(o.includePayloads, true);
  for (const k of ['url', 'headers', 'auth']) assert.equal(k in o, false, k);
  // Options are copies: a scenario mutating them does not change the config.
  o.args.push('x');
  o.env.X = '1';
  assert.deepEqual(clientOptions().args, ['server.mjs', '--stdio']);
  assert.deepEqual(clientOptions().env, { PERSONA: 'leaky' });
});

test('requireHttp', async () => {
  const http = await load({});
  assert.doesNotThrow(() => http.requireHttp('lb-check', 'why'));
  const stdio = await load({ MCP_COMMAND: '["node"]' });
  assert.throws(() => stdio.requireHttp('lb-check', 'why'), /lb-check needs an HTTP target/);
});
