// requires k6 built with xk6-mcpload
//
// oauth-refresh: many VUs on short-lived OAuth tokens (client credentials). Measures token refresh
// storms (mcp_oauth_refresh_duration) and 401s (mcp_errors{error_type:auth}).
// Demo pair: mock-oauth (30 s tokens) on :3006, ts-oauth on :3007. Those are also the defaults here.
//
//   ./k6 run -e MCP_URL=http://localhost:3007/mcp \
//            -e OAUTH_TOKEN_URL=http://localhost:3006/token \
//            -e OAUTH_CLIENT_ID=mcpload -e OAUTH_CLIENT_SECRET=secret scenarios/oauth-refresh.js
//   VUS (50), DURATION (3m, several token lifetimes), REFRESH_P95_MS (500), AUTH_MAX_ERRORS (1)
import { buildThresholds, config, env, envNum } from './lib/config.js';
import { agentSession, makeClient } from './lib/session.js';

const auth =
  config.auth && config.auth.type === 'oauth'
    ? config.auth
    : {
        type: 'oauth',
        tokenUrl: env('OAUTH_TOKEN_URL', 'http://localhost:3006/token'),
        clientId: env('OAUTH_CLIENT_ID', 'mcpload'),
        clientSecret: env('OAUTH_CLIENT_SECRET', 'secret'),
      };
const client = makeClient({ auth, url: env('MCP_URL', 'http://localhost:3007/mcp') });

const VUS = envNum('VUS', 50);

export const options = {
  scenarios: {
    oauth: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { target: VUS, duration: '15s' },
        { target: VUS, duration: env('DURATION', '3m') },
        { target: 0, duration: '10s' },
      ],
      gracefulRampDown: '15s',
    },
  },
  thresholds: buildThresholds({
    mcp_oauth_refresh_duration: [`p(95)<${envNum('REFRESH_P95_MS', 500)}`],
    'mcp_errors{error_type:auth}': [`count<${envNum('AUTH_MAX_ERRORS', 1)}`],
  }),
  tags: { scenario_name: 'oauth-refresh' },
};

export default function () {
  // Short sessions, several per token lifetime, so refreshes happen mid-traffic.
  agentSession(client, { rounds: 2 });
}
