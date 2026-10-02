// Smoke test for the k6/x/mcpload extension.
//
//   ./k6.exe run -e MCP_URL=http://localhost:3001/mcp xk6-mcpload/examples/smoke.js
//
// Optional env: MCP_PROTOCOL (default auto), MCP_TOOL / MCP_ARGS (JSON) to pick
// the tool to call, MCP_TOKEN (bearer), OAUTH_TOKEN_URL / OAUTH_CLIENT_ID /
// OAUTH_CLIENT_SECRET (client_credentials).
import mcp from 'k6/x/mcpload';
import { check } from 'k6';

export const options = {
  vus: Number(__ENV.VUS || 2),
  iterations: Number(__ENV.ITERATIONS || 6),
  thresholds: {
    mcp_req_duration: ['p(95)<2000'],
    'mcp_errors{error_type:session_not_found}': ['count<1'],
  },
};

function auth() {
  if (__ENV.OAUTH_TOKEN_URL) {
    return {
      type: 'oauth',
      tokenUrl: __ENV.OAUTH_TOKEN_URL,
      clientId: __ENV.OAUTH_CLIENT_ID,
      clientSecret: __ENV.OAUTH_CLIENT_SECRET,
    };
  }
  if (__ENV.MCP_TOKEN) return { type: 'bearer', token: __ENV.MCP_TOKEN };
  return undefined;
}

const client = new mcp.Client({
  url: __ENV.MCP_URL || 'http://localhost:3001/mcp',
  protocol: __ENV.MCP_PROTOCOL || 'auto',
  timeout: '10s',
  auth: auth(),
});

export default function () {
  const s = client.connect();
  const tools = s.listTools();
  check(tools, { 'tools listed': (t) => t.length > 0 });

  const tool = __ENV.MCP_TOOL || (tools.find((t) => /echo|fast/.test(t.name)) || tools[0]).name;
  const args = __ENV.MCP_ARGS ? JSON.parse(__ENV.MCP_ARGS) : {};

  const r = s.callTool(tool, args);
  check(r, { 'tool ok': (x) => !x.isError });
  if (r.error) console.warn(`callTool ${tool}: ${r.error.type}: ${r.error.message}`);

  const par = s.callParallel([
    { name: tool, args },
    { name: tool, args },
    { name: tool, args },
  ]);
  check(par, { 'parallel all ok': (a) => a.length === 3 && a.every((x) => !x.isError) });

  s.ping();
  if (__ITER === 0 && __VU === 1) {
    console.log(`protocol=${s.protocol} sessionId=${s.sessionId} tools=${tools.map((t) => t.name).join(',')}`);
  }
  s.close();
}
