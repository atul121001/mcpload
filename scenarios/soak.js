// requires k6 built with xk6-mcpload
//
// soak: agent sessions at a constant arrival rate for SOAK_MIN minutes, with a warm-up ramp before
// and a zero-load cool-down after. k6 stays alive during cool-down so the mcpload CLI keeps sampling.
//
//   phase       window (seconds from start)                 load
//   warm-up     [0, W)          W = WARMUP_MIN*60           ramps 0 -> RATE sessions/s
//   load        [W, W+L)        L = SOAK_MIN*60             constant RATE sessions/s
//   cool-down   [W+L, W+L+C)    C = COOLDOWN_MIN*60         none (one idle VU sleeps)
// The CLI derives report.json `phases` from the same env vars (defaults below).
// In-flight sessions may finish up to 30 s (gracefulStop) into cool-down.
//
//   ./k6 run -e MCP_URL=http://localhost:3002/mcp -e SOAK_MIN=30 scenarios/soak.js
//   SOAK_MIN (30), WARMUP_MIN (10% of SOAK_MIN, min 1), COOLDOWN_MIN (5),
//   RATE sessions/s (2), PRE_VUS (20), MAX_VUS (200)
import { sleep } from 'k6';
import { buildThresholds, envNum, secs } from './lib/config.js';
import { agentSession, makeClient } from './lib/session.js';

const client = makeClient();

const SOAK_MIN = envNum('SOAK_MIN', 30);
const WARMUP_MIN = envNum('WARMUP_MIN', Math.max(1, SOAK_MIN * 0.1));
const COOLDOWN_MIN = envNum('COOLDOWN_MIN', 5);
const RATE = envNum('RATE', 2);
const PRE_VUS = envNum('PRE_VUS', 20);
const MAX_VUS = envNum('MAX_VUS', 200);

const W = Math.round(WARMUP_MIN * 60);
const L = Math.round(SOAK_MIN * 60);
const C = Math.round(COOLDOWN_MIN * 60);

const phases = { warmupEndS: W, loadEndS: W + L, cooldownEndS: W + L + C };

const scenarios = {
  load: {
    executor: 'constant-arrival-rate',
    exec: 'session',
    startTime: secs(W),
    rate: RATE,
    timeUnit: '1s',
    duration: secs(L),
    preAllocatedVUs: PRE_VUS,
    maxVUs: MAX_VUS,
    gracefulStop: '30s',
  },
};
if (W > 0) {
  scenarios.warmup = {
    executor: 'ramping-arrival-rate',
    exec: 'session',
    startRate: 0,
    timeUnit: '1s',
    preAllocatedVUs: PRE_VUS,
    maxVUs: MAX_VUS,
    stages: [{ target: RATE, duration: secs(W) }],
    gracefulStop: '30s',
  };
}
if (C > 0) {
  scenarios.cooldown = {
    executor: 'per-vu-iterations',
    exec: 'idle',
    startTime: secs(W + L),
    vus: 1,
    iterations: 1,
    maxDuration: secs(C + 60),
    gracefulStop: '0s',
  };
}

export const options = {
  scenarios,
  thresholds: buildThresholds(),
  tags: { scenario_name: 'soak' },
  summaryTrendStats: ['avg', 'min', 'med', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  console.log(`soak phases (s): warm-up 0-${phases.warmupEndS}, load ${phases.warmupEndS}-${phases.loadEndS}, cool-down ${phases.loadEndS}-${phases.cooldownEndS}; rate ${RATE} sessions/s`);
  return phases;
}

export function session() {
  agentSession(client);
}

// Cool-down: no MCP traffic at all; keep k6 (and the CLI's sampler) running.
export function idle() {
  sleep(C);
}
