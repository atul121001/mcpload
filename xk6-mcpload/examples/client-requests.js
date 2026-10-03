// Server-to-client requests: answer sampling and elicitation inside tools/call streams.
//
//   ./k6.exe run -e MCP_URL=http://localhost:3001/mcp xk6-mcpload/examples/client-requests.js
//
// Needs a stateful server whose tools send sampling/createMessage or elicitation/create on the
// tools/call stream: the TypeScript demo server's `sample_llm` and `elicit_input` tools (port 3001).
// Optional env: SAMPLING_DELAY_MS (default 200) and ELICITATION_DELAY_MS (default 100) simulate the
// time an LLM or a human takes to answer.
import mcp from 'k6/x/mcpload';
import { check } from 'k6';

export const options = {
  vus: Number(__ENV.VUS || 5),
  duration: __ENV.DURATION || '20s',
  thresholds: {
    'mcp_tool_error_rate': ['rate<0.01'],
    'mcp_server_requests{method:sampling/createMessage}': ['count>0'],
    'mcp_server_request_duration{method:sampling/createMessage}': ['p(95)<1000'],
    'mcp_errors{error_type:unsupported_request}': ['count<1'],
  },
};

const client = new mcp.Client({
  url: __ENV.MCP_URL || 'http://localhost:3001/mcp',
  protocol: __ENV.MCP_PROTOCOL || 'auto',
  timeout: '10s',
  sampling: {
    response: { role: 'assistant', content: { type: 'text', text: 'ok' }, model: 'mcpload-mock', stopReason: 'endTurn' },
    delayMs: Number(__ENV.SAMPLING_DELAY_MS || 200),
  },
  elicitation: { action: 'accept', content: { confirm: true, note: 'from mcpload' }, delayMs: Number(__ENV.ELICITATION_DELAY_MS || 100) },
});

export default function () {
  const s = client.connect();
  const rs = s.callParallel([
    { name: 'sample_llm', args: { prompt: 'Summarise the results.' } },
    { name: 'elicit_input', args: {} },
    { name: 'fast', args: {} },
  ]);
  check(rs, { 'all ok': (a) => a.every((r) => !r.isError) });
  for (const r of rs) if (r.error) console.warn(`${r.error.type}: ${r.error.message}`);
  if (__ITER === 0 && __VU === 1) console.log(rs.map((r) => (r.content[0] || {}).text).join(' | '));
  s.close();
}
