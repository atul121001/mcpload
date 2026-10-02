// requires k6 built with xk6-mcpload
//
// burst: many agents start at once.
//   init_flood  arrival-rate flood of bare connect/close (initialize + DELETE, or server/discover):
//               the "initialize storm" class of bugs.
//   agents      VUs ramp from 0 to BURST_VUS within RAMP, hold, then drop.
//
//   ./k6 run -e MCP_URL=http://localhost:3001/mcp -e BURST_VUS=200 scenarios/burst.js
//   BURST_VUS (200), RAMP (10s), HOLD (30s), FLOOD_RATE connects/s (100), FLOOD_VUS max (300),
//   FLOOD_CONNECT_P95_MS (2000), FLOOD_MAX_ERRORS (1)
import { check } from 'k6';
import { buildThresholds, env, envNum } from './lib/config.js';
import { agentSession, makeClient } from './lib/session.js';

const client = makeClient();

const BURST_VUS = envNum('BURST_VUS', 200);
const RAMP = env('RAMP', '10s');
const HOLD = env('HOLD', '30s');
const FLOOD_RATE = envNum('FLOOD_RATE', 100);
const FLOOD_VUS = envNum('FLOOD_VUS', 300);

export const options = {
  scenarios: {
    init_flood: {
      executor: 'ramping-arrival-rate',
      exec: 'initFlood',
      startRate: 0,
      timeUnit: '1s',
      preAllocatedVUs: Math.min(FLOOD_VUS, Math.max(10, FLOOD_RATE)),
      maxVUs: FLOOD_VUS,
      stages: [
        { target: FLOOD_RATE, duration: '5s' },
        { target: FLOOD_RATE, duration: '15s' },
        { target: 0, duration: '5s' },
      ],
      gracefulStop: '15s',
    },
    agents: {
      executor: 'ramping-vus',
      exec: 'agents',
      startVUs: 0,
      startTime: '25s',
      stages: [
        { target: BURST_VUS, duration: RAMP },
        { target: BURST_VUS, duration: HOLD },
        { target: 0, duration: '5s' },
      ],
      gracefulRampDown: '15s',
    },
  },
  thresholds: buildThresholds({
    // k6 adds the `scenario` tag to every sample, so the flood can be budgeted on its own.
    'mcp_connect_duration{scenario:init_flood}': [`p(95)<${envNum('FLOOD_CONNECT_P95_MS', 2000)}`],
    'mcp_errors{scenario:init_flood}': [`count<${envNum('FLOOD_MAX_ERRORS', 1)}`],
  }),
  tags: { scenario_name: 'burst' },
};

export function initFlood() {
  let s;
  try {
    s = client.connect();
  } catch (e) {
    check(null, { 'flood connect ok': () => false });
    return;
  }
  check(s, { 'flood connect ok': (x) => !!x });
  try {
    s.close();
  } catch (e) {
    // counted in mcp_errors by the extension
  }
}

export function agents() {
  agentSession(client);
}
