// requires k6 built with xk6-mcpload
//
// isolation: do fast tools get slower when slow tools run at the same time?
//
// A server that funnels every tool through one shared resource (a DB or HTTP connection pool, a worker pool,
// a blocked event loop) makes its fast tools wait behind its slow ones. Each tool's own latency looks fine on
// its own, so per-tool budgets rarely catch it. This scenario runs the same agent sessions twice, at the same
// concurrency:
//
//   solo   the tool mix without SLOW_TOOLS
//   mixed  the full tool mix, SLOW_TOOLS included
//
// mcpload then compares every tool's p95 between the two phases (verdict tool_isolation).
//
//   ./mcpload run --url http://localhost:3001/mcp --scenario isolation                        # ts-healthy: expected PASS
//   ./mcpload run --url http://localhost:3008/mcp --scenario isolation                        # ts-pooled:  expected FAIL
//   ./mcpload run --url ... --scenario isolation --env SLOW_TOOLS=generate_report,export_csv
//
//   VUS (20), DURATION per phase (1m), SLOW_TOOLS comma-separated (default "slow", the demo servers' slow tool)
import { check } from 'k6';
import { config, buildThresholds, durationSeconds, env, envNum } from './lib/config.js';
import { agentSession, makeClient } from './lib/session.js';
import { withoutTools } from './lib/tools.js';

const client = makeClient();
const SLOW = env('SLOW_TOOLS', 'slow')
  .split(',')
  .map((s) => s.trim())
  .filter(Boolean);
const PHASE_S = Math.max(1, Math.round(durationSeconds(env('DURATION', '1m'))));
const VUS = envNum('VUS', 20);
const STOP_S = 5; // gracefulStop of the solo phase; mixed starts only after it

export const options = {
  scenarios: {
    // The scenario names are the contract with mcpload's tool_isolation verdict.
    solo: { executor: 'constant-vus', exec: 'solo', vus: VUS, duration: `${PHASE_S}s`, gracefulStop: `${STOP_S}s` },
    mixed: {
      executor: 'constant-vus',
      exec: 'mixed',
      vus: VUS,
      duration: `${PHASE_S}s`,
      startTime: `${PHASE_S + STOP_S + 2}s`,
      gracefulStop: '30s',
    },
  },
  thresholds: buildThresholds({
    // A SLOW_TOOLS typo would make both phases identical and the comparison meaningless.
    'checks{check:slow tools in mix}': ['rate>0.999'],
  }),
  tags: { scenario_name: 'isolation' },
};

export function setup() {
  if (!SLOW.length) throw new Error('SLOW_TOOLS is empty: name the slow tool(s) to compare against');
  console.log(
    `isolation -> ${config.target}: ${PHASE_S}s solo (mix without ${SLOW.join(', ')}), then ${PHASE_S}s mixed, ${VUS} VUs each`,
  );
}

export function solo() {
  agentSession(client, { tableFilter: (tt) => withoutTools(tt, SLOW) });
}

let warnedSlow = false;

export function mixed() {
  agentSession(client, {
    tableFilter: (tt) => {
      const present = tt.table.some((e) => SLOW.indexOf(e.name) >= 0);
      check(null, { 'slow tools in mix': () => present });
      if (!present && !warnedSlow) {
        warnedSlow = true;
        console.warn(`[vu ${__VU}] SLOW_TOOLS (${SLOW.join(', ')}) not in the tool mix; mix: ${tt.table.map((e) => e.name).join(', ')}`);
      }
      return tt;
    },
  });
}
