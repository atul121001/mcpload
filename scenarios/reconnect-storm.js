// requires k6 built with xk6-mcpload
//
// reconnect-storm: VUS agents each hold a session and call tools with think time. When a session breaks
// (session_not_found, or every call of a round failing with a transport error), the agent reconnects at once,
// then backs off between failed attempts. Run it with `mcpload run --chaos-restart 30s --chaos-container <name>`:
// mcpload restarts that container mid-run and the `recovery` verdict measures how long the server takes to
// serve normally again while every agent reconnects at the same moment (the storm).
//
// Every tools/call carries a call id in params._meta["io.mcpload/callId"] ("<prefix>-<vu>-<n>"). A server that
// records the ids it executes (the TS demo with TRACK_CALLS=1, GET /calls) lets mcpload compare what ran with
// what the client saw (`--calls-url`, verdict call_integrity): calls that failed on the client but ran on the
// server, calls that never ran, and calls that ran twice.
//
//   ./mcpload run --scenario reconnect-storm --url http://localhost:3019/mcp \
//     --chaos-restart 30s --chaos-container mcpload-chaos-ts --calls-url http://localhost:3019/calls
//   VUS (20), DURATION (2m), RECONNECT_BACKOFF_MS (100): wait before reconnect attempt n >= 2, doubling up to
//   RECONNECT_MAX_MS (5000); 0 = retry in a hot loop. JITTER (0): fraction of each wait taken off at random.
//   RETRY_ON_ERROR (0): 1 re-sends a call that failed with a transport error or session_not_found on the next
//   session, with the SAME call id, up to RETRY_MAX attempts in all (3): the "client retry fires a
//   non-idempotent call twice" case. DOUBLE_SEND (0): share of calls sent twice at once with the same id (a
//   hedged request, or two workers picking the same job). CALL_ID_PREFIX (random per run; mcpload sets it).
//   Read by mcpload: RECOVERY_BUDGET seconds (30), RECOVERY_WINDOW seconds (5), ERR_RATE, CONNECT_P95_MS.
// No k6 thresholds: errors during the outage are the point of the test; the recovery verdict judges it.
import exec from 'k6/execution';
import { check, sleep } from 'k6';
import { Counter, Trend } from 'k6/metrics';
import { config, durationSeconds, env, envNum, thinkSeconds } from './lib/config.js';
import { backoffMs, breakCause, CALL_ID_KEY, callIdFactory, retryable } from './lib/resilience.js';
import { callTools, checkToolTable, makeClient, pick, toolTable } from './lib/session.js';

const client = makeClient();

const VUS = Math.max(1, Math.floor(envNum('VUS', 20)));
const DURATION = env('DURATION', '2m');
const BACKOFF_MS = envNum('RECONNECT_BACKOFF_MS', 100);
const BACKOFF_MAX_MS = envNum('RECONNECT_MAX_MS', 5000);
const JITTER = envNum('JITTER', 0);
const RETRY_ON_ERROR = ['1', 'true', 'yes'].indexOf(String(env('RETRY_ON_ERROR', '0')).toLowerCase()) >= 0;
const RETRY_MAX = Math.max(1, Math.floor(envNum('RETRY_MAX', 3)));
const DOUBLE_SEND = envNum('DOUBLE_SEND', 0);

const breaks = new Counter('mcp_session_breaks');
const reconnects = new Counter('mcp_reconnects');
const reconnectTime = new Trend('mcp_reconnect_duration', true);
const tagged = new Counter('mcp_calls_tagged');
// One sample per attempt of a call id that failed or was sent more than once (tags call_id, outcome).
const attempts = new Counter('mcp_call_attempts');

export const options = {
  scenarios: {
    agents: { executor: 'constant-vus', vus: VUS, duration: DURATION, gracefulStop: '10s' },
  },
  tags: { scenario_name: 'reconnect-storm' },
  summaryTrendStats: ['avg', 'min', 'med', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  let prefix = env('CALL_ID_PREFIX', '');
  if (!prefix) for (let i = 0; i < 8; i++) prefix += Math.floor(Math.random() * 16).toString(16);
  console.log(`reconnect-storm: ${VUS} agents -> ${config.url} for ${DURATION}; reconnect backoff ${BACKOFF_MS} ms (max ${BACKOFF_MAX_MS}, jitter ${JITTER}); retry on error ${RETRY_ON_ERROR ? `on (${RETRY_MAX} attempts)` : 'off'}; call ids ${prefix}-<vu>-<n>`);
  return { prefix };
}

// Per-VU state, kept across iterations (one iteration = one session).
let nextId;
let failedConnects = 0;
let brokeAt = 0;
let pending = []; // calls to retry on the next session: {name, args, ownBudget, meta, id, attempt}

let loggedErrors = 0;
function logError(msg) {
  if (loggedErrors++ < 20) console.warn(`[vu ${__VU}] ${msg}`);
}

function newCalls(tt) {
  const out = [];
  for (let i = 0; i < config.parallel; i++) {
    const e = pick(tt);
    const id = nextId();
    tagged.add(1);
    const c = { name: e.name, args: e.args, ownBudget: e.ownBudget, meta: { [CALL_ID_KEY]: id }, id, attempt: 1 };
    out.push(c);
    if (DOUBLE_SEND > 0 && Math.random() < DOUBLE_SEND) {
      c.doubled = true;
      out.push(Object.assign({}, c, { attempt: 2 }));
    }
  }
  return out;
}

export default function (data) {
  if (!nextId) nextId = callIdFactory(data.prefix, exec.vu.idInTest);
  const endAt = exec.scenario.startTime + durationSeconds(DURATION) * 1000;
  if (failedConnects > 0) sleep(backoffMs(failedConnects + 1, BACKOFF_MS, BACKOFF_MAX_MS, JITTER) / 1000);
  if (Date.now() >= endAt) return;

  let s;
  try {
    s = client.connect();
  } catch (e) {
    failedConnects++;
    check(null, { 'connect ok': () => false });
    logError(`connect failed: ${e}`);
    return;
  }
  check(s, { 'connect ok': () => true });
  failedConnects = 0;
  if (brokeAt) {
    reconnects.add(1);
    reconnectTime.add(Date.now() - brokeAt);
    brokeAt = 0;
  }

  let broken = '';
  try {
    const tt = toolTable(s.listTools());
    if (!checkToolTable(tt, logError)) return;
    while (Date.now() < endAt) {
      const batch = pending.length ? pending : newCalls(tt);
      pending = [];
      const results = callTools(s, batch);
      for (let i = 0; i < batch.length; i++) {
        const c = batch[i];
        const err = results[i].error;
        const failed = !!err && err.type !== 'tool_iserror';
        if (failed) logError(`${c.name} ${c.id}: ${err.type}: ${err.message}`);
        if (failed || c.attempt > 1 || c.doubled) attempts.add(1, { call_id: c.id, outcome: failed ? err.type : 'answered' });
      }
      check(results, { 'tools/call no transport error': (rs) => rs.every((x) => !x.error || x.error.type === 'tool_iserror') });
      // A retrying client re-sends every call that failed in transit, on this session or (when it broke) the next.
      if (RETRY_ON_ERROR) {
        for (let i = 0; i < batch.length; i++) {
          const c = batch[i];
          if (!c.doubled && retryable(results[i].error) && c.attempt < RETRY_MAX) pending.push(Object.assign({}, c, { attempt: c.attempt + 1 }));
        }
      }
      broken = breakCause(results.map((r) => r.error || null));
      if (broken) break;
      sleep(thinkSeconds());
    }
  } catch (e) {
    logError(`session error: ${e}`);
    broken = e.type || 'error';
  } finally {
    // A broken session is abandoned without a DELETE, as most clients do: the server has lost it or is down.
    if (!broken) {
      try {
        s.close();
      } catch (e) {
        logError(`close failed: ${e}`);
      }
    }
  }
  if (broken) {
    breaks.add(1, { cause: broken });
    brokeAt = Date.now();
  }
}
