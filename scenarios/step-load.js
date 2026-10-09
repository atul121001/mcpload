// requires k6 built with xk6-mcpload
//
// step-load: how many concurrent agents does the server hold before it breaks its budgets?
//
// Agent sessions (as in agent-session.js) with concurrency rising in steps. Each step ramps for RAMP and then
// holds its level for STEP_DURATION. Every request made during a hold is tagged step=<VUs of that step>
// (ramps are left untagged), so mcpload can compute per-step, per-tool p95/p99 and error rates and report the
// last step that stayed within P95_MS / P99_MS / ERR_RATE / TOOL_BUDGETS / CONNECT_P95_MS (verdict capacity).
//
//   ./mcpload run --url http://localhost:3001/mcp --scenario step-load
//   ./mcpload run --url ... --scenario step-load --env STEPS=5,10,20,40 --env STEP_DURATION=20s --min-agents 20
//
//   STEPS            comma-separated VU levels (default 10,25,50,100,200)
//   START, STEP_FACTOR, MAX_VUS   used instead when STEPS is unset and any of them is set: START, START*STEP_FACTOR,
//                    ... up to MAX_VUS (defaults 10, 2, 200)
//   STEP_DURATION    hold per step (default 1m); RAMP ramp before each step (default 5s)
//   ABORT_ERR_RATE   stop the whole run once a VU sees more than this share of its calls fail within one step
//                    (default 0.5; 0 turns the guard off), after at least ABORT_MIN_CALLS calls (default 20).
//                    Saves hammering a server that has already fallen over. k6 then exits with code 108.
//   MIN_AGENTS       read by mcpload, not by this script: the concurrency the server must hold for a PASS.
//
// The script sets no k6 thresholds: budgets are judged per step by mcpload, and a breach at the top step is the
// expected outcome of a step-load run, not a failure of the run.
import exec from 'k6/execution';
import { config, durationSeconds, env, envNum } from './lib/config.js';
import { agentSession, makeClient } from './lib/session.js';
import { stepAt, stepLevels, stepSchedule } from './lib/steps.js';

const client = makeClient();

const geometric = ['START', 'STEP_FACTOR', 'MAX_VUS'].some((k) => env(k, undefined) !== undefined);
const LEVELS = stepLevels({
  steps: env('STEPS', geometric ? undefined : '10,25,50,100,200'),
  start: envNum('START', 10),
  factor: envNum('STEP_FACTOR', 2),
  max: envNum('MAX_VUS', 200),
});
const HOLD_S = Math.max(1, Math.round(durationSeconds(env('STEP_DURATION', '1m'))));
const RAMP_S = Math.max(1, Math.round(durationSeconds(env('RAMP', '5s'))));
const SCHED = stepSchedule(LEVELS, HOLD_S, RAMP_S);
const ABORT_ERR_RATE = envNum('ABORT_ERR_RATE', 0.5);
const ABORT_MIN_CALLS = Math.max(1, envNum('ABORT_MIN_CALLS', 20));

export const options = {
  scenarios: {
    // The step tag (not the scenario name) is the contract with mcpload's capacity verdict.
    steps: { executor: 'ramping-vus', startVUs: 0, stages: SCHED.stages, gracefulRampDown: '30s', gracefulStop: '30s' },
  },
  thresholds: {},
  tags: { scenario_name: 'step-load' },
};

export function setup() {
  console.log(
    `step-load -> ${config.target}: steps ${LEVELS.join(', ')} VUs, ${HOLD_S}s each after a ${RAMP_S}s ramp (${SCHED.totalS}s)` +
      (ABORT_ERR_RATE > 0 ? `, stop when a VU sees > ${ABORT_ERR_RATE * 100}% failed calls in a step` : ''),
  );
}

/** Tag the VU with the step being held now (none during ramps); returns the step's window or null. */
function tagStep() {
  const w = stepAt(SCHED.windows, (Date.now() - exec.scenario.startTime) / 1000);
  const tags = exec.vu.metrics.tags;
  if (w) tags.step = String(w.vus);
  else delete tags.step;
  return w;
}

// Per-VU tally of the current step for the abort guard.
let guard = { vus: 0, calls: 0, errors: 0 };

export default function () {
  const w = tagStep();
  const out = agentSession(client, { beforeRound: tagStep });
  if (!w || !(ABORT_ERR_RATE > 0)) return;
  if (guard.vus !== w.vus) guard = { vus: w.vus, calls: 0, errors: 0 };
  // A session that failed before its first call (connect, tools/list) counts as one failed call.
  guard.calls += out.calls || 1;
  guard.errors += out.calls ? out.toolErrors : out.ok ? 0 : 1;
  if (guard.calls >= ABORT_MIN_CALLS && guard.errors / guard.calls > ABORT_ERR_RATE) {
    exec.test.abort(
      `step-load: stopping at ${w.vus} VUs: ${guard.errors} of ${guard.calls} calls failed on VU ${__VU} (ABORT_ERR_RATE=${ABORT_ERR_RATE})`,
    );
  }
}
