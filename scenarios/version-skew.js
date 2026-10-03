// requires k6 built with xk6-mcpload
//
// version-skew: what do clients see while a rolling deploy has replicas on two different builds behind one
// load balancer? Point it at the LB. Each request can land on either build, so a session negotiated with one
// build sends some of its requests to the other one. That is harmless when both builds speak the same protocol
// and list the same tools; when they don't, the question that matters is HOW requests fail:
//
//   fast  a typed error, returned quickly (HTTP 4xx/5xx, JSON-RPC -32022 Unsupported protocol version,
//         -32601 Method not found, an "unknown tool" error): a client can catch it, retry or reconnect
//   hang  no answer until the client's timeout (MCP_TIMEOUT), or a failure that took HANG_MS or longer:
//         every client stalls for the length of the deploy
//   slow  anything in between: a typed error slower than FAIL_FAST_MS, or a failure without an HTTP status
//
// Each VU loops through agent sessions: connect (protocol MCP_PROTOCOL, default auto, so every session goes
// through version negotiation and its retry path), tools/list, STEPS sequential tools/call (each its own trip
// through the LB), close. Requests that fail are classified as above; ordinary tool errors (isError results
// such as the demo `flaky` tool) are not failures. A session ends at its first hang, as an agent would give up.
//
// With TOOLS_CACHE_TTL (e.g. 5m) a VU reuses the tools/list of an earlier session for that long instead of
// listing again, like a client that honours a list's cache TTL; it may then call a tool (e.g. `new_tool`) the
// replica serving the call doesn't have. The tool mix is uniform over every listed tool unless TOOL_MIX is set.
//
// Which replica answered comes from the SERVED_BY_HEADER response header (default X-Served-By; nginx's
// X-Upstream works too). mcpload turns the metrics into the version_skew verdict: pass without failures, warn
// when every failure was fast, fail on any hang.
//
//   ./mcpload run --url http://localhost:3009/mcp --scenario version-skew                     # skew: expected WARN
//   ./mcpload run --url http://localhost:3010/mcp --scenario version-skew --env MCP_TIMEOUT=5s # skew-hang: FAIL
//   ./mcpload run --url http://localhost:3005/mcp --scenario version-skew                     # one build: PASS
//
//   VUS (10), DURATION (1m), STEPS tools/call per session (6), TOOLS_CACHE_TTL (off), FAIL_FAST_MS (2000),
//   HANG_MS (10000), SERVED_BY_HEADER (X-Served-By), REMEMBER_PROTOCOL (0: every session negotiates; 1: reuse
//   the protocol resolved by an earlier connect, mcpload's default elsewhere)
//
// Metrics (all tagged op: connect|list|call|close):
//   mcp_skew_requests{outcome:ok|fail, replica, protocol}    every request the scenario made (replica no-answer:
//                                                            a timeout or connection error; no tag: not known)
//   mcp_skew_failures{kind:fast|slow|hang, reason}            failed requests (reason: lib/skew.js#failureReason)
//   mcp_skew_failure_duration{kind, reason}                   time until the failure surfaced (ms)
//   mcp_skew_negotiated{protocol, replica}                    successful connects, by negotiated protocol and the
//                                                            replica that answered the handshake (cached: none sent)
import { check, sleep } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import { config, env, envNum } from './lib/config.js';
import { makeClient } from './lib/session.js';
import { planTools, pick } from './lib/tools.js';
import { classifyFailure, parseTtlMs, ToolListCache } from './lib/skew.js';

const STEPS = Math.max(1, Math.floor(envNum('STEPS', 6)));
const FAIL_FAST_MS = envNum('FAIL_FAST_MS', 2000);
const HANG_MS = envNum('HANG_MS', 10000);
const TTL_MS = parseTtlMs(env('TOOLS_CACHE_TTL', ''));
const SERVED_BY_HEADER = env('SERVED_BY_HEADER', 'X-Served-By');
const REMEMBER = ['1', 'true', 'yes'].indexOf(String(env('REMEMBER_PROTOCOL', '0')).toLowerCase()) >= 0;

const client = makeClient({ rememberProtocol: REMEMBER, servedByHeader: SERVED_BY_HEADER });

const skewRequests = new Counter('mcp_skew_requests');
const skewFailures = new Counter('mcp_skew_failures');
const skewFailureDuration = new Trend('mcp_skew_failure_duration', true);
const skewNegotiated = new Counter('mcp_skew_negotiated');
const skewOk = new Rate('mcp_skew_ok');

export const options = {
  scenarios: {
    skew: {
      executor: 'constant-vus',
      vus: envNum('VUS', 10),
      duration: env('DURATION', '1m'),
      gracefulStop: '30s',
    },
  },
  // No latency or error budgets: failures are expected under skew, and mcpload judges how they fail
  // (verdict version_skew). k6 alone fails the run on any hang, or when no request succeeded at all.
  thresholds: {
    'mcp_skew_failures{kind:hang}': ['count<1'],
    mcp_skew_ok: ['rate>0'],
  },
  tags: { scenario_name: 'version-skew' },
};

const cache = new ToolListCache(TTL_MS);

let warned = 0;
function warn(msg) {
  if (warned++ < 10) console.warn(`[vu ${__VU}] ${msg}`);
}

/** Record one request; returns its classification (null when it did not fail). */
function record(op, err, ms, replica, protocol, what) {
  const c = classifyFailure(err, ms, { failFastMs: FAIL_FAST_MS, hangMs: HANG_MS });
  const tags = { op, outcome: c ? 'fail' : 'ok' };
  if (replica) tags.replica = replica;
  else if (c && !c.typed) tags.replica = 'no-answer'; // timeout / connection error: no response to name a replica
  if (protocol) tags.protocol = protocol;
  skewRequests.add(1, tags);
  skewOk.add(!c, { op });
  check(null, { 'skew: request ok': () => !c, 'skew: no hang': () => !c || c.kind !== 'hang' });
  if (c) {
    skewFailures.add(1, { op, kind: c.kind, reason: c.reason });
    skewFailureDuration.add(ms, { op, kind: c.kind, reason: c.reason });
    warn(`${what || op} -> ${c.kind} ${c.reason} after ${Math.round(ms)} ms (replica ${replica || 'unknown'}, protocol ${protocol || 'none'}): ${err.message}`);
  }
  return c;
}

function toolTable(listed) {
  if (config.toolMixExplicit) return planTools(listed, config);
  const mix = {};
  for (const t of listed) if (t && typeof t.name === 'string') mix[t.name] = 1;
  return planTools(listed, { toolMix: mix, toolArgs: config.toolArgs, userBudgets: {} });
}

export function setup() {
  console.log(
    `version-skew -> ${config.url} (protocol ${config.protocol}${REMEMBER ? ', remembered across sessions' : ', negotiated per session'}, ` +
      `${STEPS} calls per session, tools/list cache ${TTL_MS ? `${TTL_MS} ms` : 'off'}, fast < ${FAIL_FAST_MS} ms, hang >= ${HANG_MS} ms or timeout ${config.timeout})`,
  );
}

export default function () {
  let t = Date.now();
  let s;
  try {
    s = client.connect();
  } catch (e) {
    record('connect', e, Date.now() - t, e.servedBy, '', 'connect');
    sleep(0.1); // don't hot-loop on a broken deploy
    return;
  }
  record('connect', null, Date.now() - t, s.servedBy, s.protocol);
  // No servedBy: REMEMBER_PROTOCOL reused a stateless resolution and sent no handshake at all.
  skewNegotiated.add(1, { protocol: s.protocol, replica: s.servedBy || (REMEMBER ? 'cached' : 'unknown') });
  try {
    let listed = cache.get(Date.now());
    if (!listed) {
      t = Date.now();
      try {
        listed = s.listTools() || [];
      } catch (e) {
        record('list', e, Date.now() - t, e.servedBy, s.protocol, 'tools/list');
        sleep(0.1);
        return;
      }
      record('list', null, Date.now() - t, '', s.protocol);
      cache.put(listed, Date.now(), s.servedBy);
    }
    const tt = toolTable(listed);
    if (!tt.table.length) {
      warn(`no tool to call (listed: ${tt.listedNames.join(', ') || 'none'})`);
      sleep(1);
      return;
    }
    for (let i = 0; i < STEPS; i++) {
      const e = pick(tt);
      const res = s.callTool(e.name, e.args);
      const c = record('call', res.error, res.durationMs, res.servedBy || (res.error && res.error.servedBy), s.protocol, `tools/call ${e.name}`);
      if (c && c.kind === 'hang') return; // the agent gives up on this session
      sleep(0.05);
    }
  } finally {
    t = Date.now();
    try {
      s.close();
    } catch (e) {
      // a stateful session's DELETE can land on the other build too
      record('close', e, Date.now() - t, e.servedBy, s.protocol, 'close');
    }
  }
}
