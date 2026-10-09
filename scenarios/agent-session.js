// requires k6 built with xk6-mcpload
//
// agent-session: VUs each run realistic agent sessions back to back
// (connect -> tools/list -> 1..N rounds of parallel tools/call -> close).
//
//   ./k6 run -e MCP_URL=http://localhost:3001/mcp scenarios/agent-session.js
//   VUS (default 10), DURATION (default 2m)
import { config, buildThresholds, env, envNum } from './lib/config.js';
import { agentSession, makeClient } from './lib/session.js';

const client = makeClient();

export const options = {
  scenarios: {
    agents: {
      executor: 'constant-vus',
      vus: envNum('VUS', 10),
      duration: env('DURATION', '2m'),
      gracefulStop: '30s',
    },
  },
  thresholds: buildThresholds(),
  tags: { scenario_name: 'agent-session' },
};

export function setup() {
  console.log(`agent-session -> ${config.target} (protocol ${config.protocol}, parallel ${config.parallel}, rounds ${config.rounds.min}-${config.rounds.max})`);
}

export default function () {
  agentSession(client);
}
