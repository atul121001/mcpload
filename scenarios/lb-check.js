// requires k6 built with xk6-mcpload
//
// lb-check: a multi-request session flow against a load-balanced deployment.
// Every request (notifications/initialized, tools/list, each tools/call, ping, DELETE) is a separate HTTP
// exchange, so a round-robin LB without sticky sessions routes some of them to a replica that never saw
// the session: 404 -> mcp_errors{error_type:session_not_found}. Header mismatches (400, -32020) are flagged too.
//
// Stateful protocols (2025-xx): the verdict is "no request lost its session".
// Stateless protocol (2026-07-28): there is no session to lose, so session_not_found cannot happen; the
// meaningful check is that every request succeeds whichever replica serves it (no http/jsonrpc/timeout/
// header_mismatch error). Replicas running different builds or tool sets show up here. That check
// ("lb request ok") runs for both protocols and is gated by LB_MIN_OK (default 0.99).
//
//   ./k6 run -e MCP_URL=http://localhost:3004/mcp scenarios/lb-check.js                               # lb-stateful: expected FAIL (exit 99)
//   ./k6 run -e MCP_URL=http://localhost:3005/mcp -e MCP_PROTOCOL=2026-07-28 scenarios/lb-check.js   # stateless-2026: expected PASS
//   VUS (10), DURATION (1m), STEPS sequential calls per session (8), LB_MIN_OK (0.99)
import { check, sleep } from 'k6';
import { config, buildThresholds, env, envNum } from './lib/config.js';
import { makeClient, toolTable, pick } from './lib/session.js';

const client = makeClient();
const STEPS = envNum('STEPS', 8);

export const options = {
  scenarios: {
    lb: {
      executor: 'constant-vus',
      vus: envNum('VUS', 10),
      duration: env('DURATION', '1m'),
      gracefulStop: '30s',
    },
  },
  thresholds: buildThresholds({
    'mcp_errors{error_type:session_not_found}': ['count<1'],
    'mcp_errors{error_type:header_mismatch}': ['count<1'],
    'checks{check:lb request ok}': [`rate>=${envNum('LB_MIN_OK', 0.99)}`],
  }),
  tags: { scenario_name: 'lb-check' },
};

let warned = 0;
function warn(msg) {
  if (warned++ < 10) console.warn(`[vu ${__VU}] ${msg}`);
}

// One check set per request. `errType` is '' on success; tool_iserror is a tool-level result, not an LB failure.
function record(errType) {
  const lbOk = !errType || errType === 'tool_iserror';
  check(null, {
    'no session_not_found': () => errType !== 'session_not_found',
    'no header_mismatch': () => errType !== 'header_mismatch',
    'lb request ok': () => lbOk,
  });
  return lbOk;
}

function errTypeOf(e) {
  return (e && e.type) || 'exception';
}

let announced = false;

export function setup() {
  console.log(`lb-check -> ${config.url} (protocol ${config.protocol}, ${STEPS} sequential calls per session)`);
}

export default function () {
  let s;
  try {
    s = client.connect();
  } catch (e) {
    // e.g. notifications/initialized routed to the other replica -> session_not_found
    record(errTypeOf(e));
    warn(`connect failed: ${e}`);
    sleep(0.05); // don't hot-loop on a broken LB
    return;
  }
  record('');
  if (!announced && __VU === 1) {
    announced = true;
    console.log(
      s.sessionId
        ? `lb-check: stateful protocol ${s.protocol}; every request must reach the replica that owns the session`
        : `lb-check: stateless protocol ${s.protocol}; no session to lose, checking every request succeeds on any replica`,
    );
  }
  try {
    let listed;
    try {
      listed = s.listTools() || [];
      record('');
    } catch (e) {
      record(errTypeOf(e));
      warn(`tools/list failed: ${e}`);
      return;
    }
    const tt = toolTable(listed);
    if (tt.table.length === 0) return;
    // Sequential, not parallel: each call is its own trip through the LB.
    for (let i = 0; i < STEPS; i++) {
      const e = pick(tt);
      const res = s.callTool(e.name, e.args);
      const errType = res.error ? res.error.type : '';
      if (!record(errType)) {
        warn(`step ${i} ${e.name}: ${errType}: ${res.error.message} (session ${s.sessionId || 'none'}, protocol ${s.protocol})`);
      }
      if (i % 3 === 2) {
        try {
          s.ping();
          record('');
        } catch (err) {
          record(errTypeOf(err));
          warn(`ping failed: ${err}`);
        }
      }
      sleep(0.05);
    }
  } finally {
    try {
      s.close();
    } catch (e) {
      // the DELETE may itself land on the wrong replica; the extension counts it in mcp_errors
    }
  }
}
