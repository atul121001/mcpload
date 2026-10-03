// requires k6 built with xk6-mcpload
//
// long-lived: each VU is one agent that opens ONE session and keeps it for a long time, calling tools with
// think time and pinging when it has been quiet, the way an IDE or chat client holds a session. A server can
// look healthy with short sessions and still drop live sessions, or slow down as a session ages.
//
//   phase       window (seconds from start)                 load
//   warm-up     [0, W)          W = WARMUP_MIN*60           VUs open their sessions, spread evenly over W
//   load        [W, W+L)        L = SESSION_MIN*60          all VUS sessions open and in use
//   cool-down   [W+L, W+L+C)    C = COOLDOWN_MIN*60         sessions closed; one idle VU sleeps
// Every session is planned to last until the end of the load phase (at least SESSION_MIN minutes). The CLI
// derives report.json `phases` from the same env vars, so the leak verdicts work as in soak.js.
//
// A session "dies" when a call or ping gets session_not_found (the server dropped it: idle reaping, a restart,
// another replica), or when DEAD_ROUNDS rounds in a row fail with transport errors only. The agent then
// reconnects (one reconnect per death) and carries on until the end of the load phase.
// Metrics: mcp_session_survived (Rate, 1 = lived to its planned end, tag cause on deaths),
// mcp_session_lifetime (Trend, ms, tags outcome=survived|died and cause), mcp_session_reconnects (Counter).
// Calls are tagged session_age=early|late in the first and last third of their session's planned lifetime,
// so mcpload can compare late with early calls of the same sessions (verdict session_survival).
//
//   ./k6 run -e MCP_URL=http://localhost:3001/mcp -e SESSION_MIN=10 scenarios/long-lived.js
//   VUS (20), SESSION_MIN (10), WARMUP_MIN (1), COOLDOWN_MIN (2), THINK_MS mean think time between rounds
//   (2000, exponential, capped at 5x), IDLE_MS extra fixed pause after every round (0), PING_EVERY ping after
//   this many quiet seconds (60; 0 = never), DEAD_ROUNDS (3), SURVIVAL_MIN share of sessions that must
//   survive (1). PARALLEL calls per round as in lib/config.js.
// Idle expiry: a server that reaps idle sessions after N s (the TS demo: SESSION_IDLE_MS, 300000) drops a
// session that stays quiet longer, so IDLE_MS above the server's idle timeout with PING_EVERY=0 shows deaths
// (session_not_found), and the same run with PING_EVERY below the timeout shows that pings keep it alive.
import exec from 'k6/execution';
import { check, sleep } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import { buildThresholds, config, envNum, secs } from './lib/config.js';
import { breakCause, sessionAge, staggerS } from './lib/resilience.js';
import { callTools, checkToolTable, makeClient, pick, toolTable } from './lib/session.js';

const client = makeClient();

const VUS = Math.max(1, Math.floor(envNum('VUS', 20)));
const SESSION_MIN = envNum('SESSION_MIN', 10);
const WARMUP_MIN = envNum('WARMUP_MIN', 1);
const COOLDOWN_MIN = envNum('COOLDOWN_MIN', 2);
const THINK_MS = envNum('THINK_MS', 2000);
const IDLE_MS = envNum('IDLE_MS', 0);
const PING_EVERY = envNum('PING_EVERY', 60);
const DEAD_ROUNDS = Math.max(1, Math.floor(envNum('DEAD_ROUNDS', 3)));
const SURVIVAL_MIN = envNum('SURVIVAL_MIN', 1);

const W = Math.round(WARMUP_MIN * 60);
const L = Math.round(SESSION_MIN * 60);
const C = Math.round(COOLDOWN_MIN * 60);
const phases = { warmupEndS: W, loadEndS: W + L, cooldownEndS: W + L + C };

const survived = new Rate('mcp_session_survived');
const lifetime = new Trend('mcp_session_lifetime', true);
const reconnects = new Counter('mcp_session_reconnects');

const scenarios = {
  agents: {
    executor: 'per-vu-iterations',
    exec: 'agent',
    vus: VUS,
    iterations: 1,
    maxDuration: secs(W + L + 60),
    gracefulStop: '30s',
  },
};
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
  thresholds: buildThresholds({ mcp_session_survived: [`rate>=${SURVIVAL_MIN}`] }),
  tags: { scenario_name: 'long-lived' },
  summaryTrendStats: ['avg', 'min', 'med', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  console.log(`long-lived: ${VUS} sessions to ${config.url}; phases (s): warm-up 0-${W}, load ${W}-${W + L}, cool-down ${W + L}-${W + L + C}; think ${THINK_MS} ms, idle ${IDLE_MS} ms, ping after ${PING_EVERY || 'never'} s quiet`);
  return phases;
}

function thinkS() {
  if (THINK_MS <= 0) return 0;
  return Math.min(-Math.log(1 - Math.random()) * THINK_MS, THINK_MS * 5) / 1000;
}

let loggedErrors = 0;
function logError(msg) {
  if (loggedErrors++ < 20) console.warn(`[vu ${__VU}] ${msg}`);
}

/**
 * Hold one session until endAt (ms) or until it dies. Returns '' when it lived to its planned end, the cause
 * (an error type) when it died, or null when it never got going (connect or tools/list failed).
 */
function holdSession(endAt) {
  let s;
  try {
    s = client.connect();
  } catch (e) {
    logError(`connect failed: ${e}`);
    check(null, { 'connect ok': () => false });
    if (config.connectBackoffMs > 0) sleep(config.connectBackoffMs / 1000);
    return null;
  }
  check(s, { 'connect ok': () => true });
  const opened = Date.now();
  const plannedS = (endAt - opened) / 1000;
  const tags = exec.vu.metrics.tags;
  let cause = '';
  let started = false;
  try {
    const listed = s.listTools();
    if (!check(listed, { 'tools/list returned tools': (l) => Array.isArray(l) && l.length > 0 })) return null;
    const tt = toolTable(listed);
    if (!checkToolTable(tt, logError)) return null;
    started = true;
    let quietSince = Date.now();
    let transportRounds = 0;
    // A transport failure counts toward DEAD_ROUNDS; session_not_found ends the session at once.
    const failed = (c) => {
      if (c === 'session_not_found') return c;
      if (!c) {
        transportRounds = 0;
        return '';
      }
      return ++transportRounds >= DEAD_ROUNDS ? c : '';
    };
    while (Date.now() < endAt && !cause) {
      const age = sessionAge((Date.now() - opened) / 1000, plannedS);
      if (age) tags.session_age = age;
      else delete tags.session_age;
      const batch = [];
      for (let i = 0; i < config.parallel; i++) batch.push(pick(tt));
      const results = callTools(s, batch);
      delete tags.session_age;
      for (let i = 0; i < results.length; i++) {
        const r = results[i];
        if (r.error && r.error.type !== 'tool_iserror') logError(`${batch[i].name}: ${r.error.type}: ${r.error.message}`);
      }
      check(results, { 'tools/call no transport error': (rs) => rs.every((x) => !x.error || x.error.type === 'tool_iserror') });
      cause = failed(breakCause(results.map((r) => r.error || null)));
      quietSince = Date.now();
      // Think, then the optional idle pause; ping whenever the session has been quiet for PING_EVERY seconds.
      let left = Math.min(thinkS() + IDLE_MS / 1000, Math.max(0, (endAt - Date.now()) / 1000));
      while (left > 0 && !cause) {
        let step = left;
        if (PING_EVERY > 0) {
          const due = PING_EVERY - (Date.now() - quietSince) / 1000;
          if (due <= 0) {
            try {
              s.ping();
              cause = failed('');
            } catch (e) {
              logError(`ping failed: ${e}`);
              cause = failed(e.type || 'http');
            }
            quietSince = Date.now();
            continue;
          }
          step = Math.min(left, due);
        }
        sleep(step);
        left -= step;
      }
    }
  } catch (e) {
    logError(`session error: ${e}`);
    cause = e.type || 'error';
  } finally {
    delete tags.session_age;
    if (!cause) {
      try {
        s.close();
      } catch (e) {
        logError(`close failed: ${e}`);
      }
    }
  }
  if (!started && !cause) return null;
  const outcome = cause ? 'died' : 'survived';
  const t = cause ? { outcome, cause } : { outcome };
  survived.add(cause ? 0 : 1, t);
  lifetime.add(Date.now() - opened, t);
  if (cause) logError(`session died after ${Math.round((Date.now() - opened) / 1000)} s: ${cause}`);
  return cause;
}

export function agent() {
  sleep(staggerS(exec.vu.idInTest, VUS, W));
  const endAt = exec.scenario.startTime + (W + L) * 1000;
  let died = false;
  while (Date.now() < endAt - 1000) {
    if (died) reconnects.add(1);
    const cause = holdSession(endAt);
    if (cause === '') return;
    died = cause !== null;
  }
}

// Cool-down: no MCP traffic at all; keep k6 (and the CLI's sampler) running.
export function idle() {
  sleep(C);
}
