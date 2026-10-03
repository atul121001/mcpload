// requires k6 built with xk6-mcpload
//
// workload: a workload profile, i.e. a realistic mix of business flows. Each VU iteration is one agent session
// that picks a flow by weight (e.g. 60% look up orders, 30% check a subscription, 10% open a ticket), takes a
// row from each test data pool, runs the flow's steps and closes:
//
//   connect -> tools/list -> step 1 -> think -> step 2 (args from step 1 and the data row) -> ... -> close
//
// Steps work as in agent-workflow (lib/workflow.js): the calls of one step are sent together (callParallel), `as`
// names a result and `$from` references feed it into later calls, `forEach`/`repeat` fan out. Unlike
// agent-workflow, a failed call (transport error or isError) ends the flow: the business task did not get done.
// A flow counts as complete when every step ran and every call succeeded.
//
// The profile is a YAML or JSON file that `mcpload run --workload <file>` validates and hands to this script as
// normalized JSON in WORKLOAD_FILE (format: lib/workload.js; source format: cmd/mcpload/internal/workload).
//
// Metrics, tagged flow (tool calls made during a flow carry the flow tag too, so mcp_req_duration{flow:...} works):
//   mcp_workload_flow_duration{flow}            one complete flow, connect() to the end of its last step, think
//                                               pauses included, ms
//   mcp_workload_flow_complete{flow}            rate of flows that completed
//   mcp_workload_step_duration{flow,flow_step}  wall time of one step's parallel batch, ms ('flow_step', not
//                                               'step': step-load tags every request with `step`)
// Thresholds come from each flow's budgets (end-to-end p95/p99, completion, per-step p95/p99) plus the usual
// per-tool budgets (TOOL_BUDGETS, else P95_MS/P99_MS/ERR_RATE).
//
//   ./mcpload run --url http://localhost:3001/mcp --workload examples/workloads/customer-support.yaml
//   VUS (the profile's agents, else 10), DURATION (its duration, else 2m), THINK_MS mean pause between steps
//   when neither the profile, the flow nor the step sets thinkMs (500)
//
// Other scenarios can run the same sessions: import { runWorkloadSession, WORKLOAD } from './workload.js'.
import { check, sleep } from 'k6';
import exec from 'k6/execution';
import { SharedArray } from 'k6/data';
import { Rate, Trend } from 'k6/metrics';
import { config, buildThresholds, env, envNum, toolBudget } from './lib/config.js';
import { makeClient } from './lib/session.js';
import { argsFromSchema } from './lib/schema-args.js';
import { DEMO_TOOL_ARGS, isDemoServer } from './lib/tools.js';
import { expandStep, failed, missingTools, newScope, recordResults, thinkSecondsFor } from './lib/workflow.js';
import { normalizeWorkload, pickFlow, pickRows, renderSteps, workloadThresholds, workloadToolNames } from './lib/workload.js';

const FILE = env('WORKLOAD_FILE', undefined);
if (!FILE) throw new Error('WORKLOAD_FILE is not set: run a workload profile with `mcpload run --workload <file.yaml>`');

export const WORKLOAD = normalizeWorkload(JSON.parse(open(FILE)));

// Data rows are held once per k6 process (SharedArray), not once per VU.
const DATA = {};
for (const name of Object.keys(WORKLOAD.data)) {
  const rows = WORKLOAD.data[name].rows;
  DATA[name] = { pick: WORKLOAD.data[name].pick, rows: new SharedArray(`workload data ${name}`, () => rows) };
  WORKLOAD.data[name].rows = null;
}

const client = makeClient();

const flowDuration = new Trend('mcp_workload_flow_duration', true);
const flowComplete = new Rate('mcp_workload_flow_complete');
const stepDuration = new Trend('mcp_workload_step_duration', true);

// Every tool the profile calls gets its own latency/error threshold, so no call needs the budget:default tag.
const toolThresholds = {};
for (const name of workloadToolNames(WORKLOAD)) {
  const tb = toolBudget(name);
  toolThresholds[`mcp_req_duration{tool:${name}}`] = [`p(95)<${tb.p95}`, `p(99)<${tb.p99}`];
  toolThresholds[`mcp_tool_error_rate{tool:${name}}`] = [`rate<${tb.errRate}`];
}

export const options = {
  scenarios: {
    workload: {
      executor: 'constant-vus',
      vus: envNum('VUS', WORKLOAD.agents || 10),
      duration: env('DURATION', WORKLOAD.duration || '2m'),
      gracefulStop: '30s',
    },
  },
  thresholds: buildThresholds(Object.assign(toolThresholds, workloadThresholds(WORKLOAD))),
  tags: { scenario_name: 'workload' },
};

export function setup() {
  const total = WORKLOAD.flows.reduce((a, f) => a + f.weight, 0);
  const flows = WORKLOAD.flows.map((f) => `${f.name} ${Math.round((100 * f.weight) / total)}% (${f.steps.map((s) => s.name).join(' -> ')})`);
  console.log(`workload ${WORKLOAD.name} -> ${config.url} (protocol ${config.protocol}): ${flows.join('; ')}`);
}

let logged = 0;
function logError(msg) {
  // Keep logs readable under load: first 20 per VU only.
  if (logged++ < 20) console.warn(`[vu ${__VU}] ${msg}`);
}

const warnedMissing = {};
let warnedDeps = false;

function backoff() {
  if (config.connectBackoffMs > 0) sleep(config.connectBackoffMs / 1000);
}

/** Base args for a tool: TOOL_ARGS, else the demo args on a demo server, else placeholders from its inputSchema. */
function baseArgsFn(listed) {
  const byName = {};
  for (const t of listed) if (t && typeof t.name === 'string') byName[t.name] = t;
  const demo = isDemoServer(Object.keys(byName));
  return (name) => {
    if (config.toolArgs[name] !== undefined) return config.toolArgs[name];
    if (demo && DEMO_TOOL_ARGS[name] !== undefined) return DEMO_TOOL_ARGS[name];
    return argsFromSchema(byName[name] && byName[name].inputSchema);
  };
}

function thinkMsFor(flow, step) {
  if (step.thinkMs !== undefined) return step.thinkMs;
  if (flow.thinkMs !== undefined) return flow.thinkMs;
  if (WORKLOAD.thinkMs !== undefined) return WORKLOAD.thinkMs;
  return config.thinkMs;
}

/** Run a flow's steps on an open session. Returns true when every step ran and every call succeeded. */
function runSteps(s, flow, steps, baseArgs) {
  const scope = newScope();
  for (let i = 0; i < steps.length; i++) {
    const step = steps[i];
    // The agent reasons about the previous step's results before deciding on the next calls.
    if (i > 0) sleep(thinkSecondsFor(thinkMsFor(flow, step), Math.random()));

    const ex = expandStep(step, scope, baseArgs);
    const missing = ex.problems.filter((p) => p.reason === 'missing');
    if (ex.hasRefs && !ex.problems.some((p) => p.reason === 'failed')) {
      check(null, { 'workload dependency resolved': () => missing.length === 0 });
    }
    if (missing.length && !warnedDeps) {
      warnedDeps = true;
      console.warn(`[vu ${__VU}] flow '${flow.name}' step '${step.name}': ${missing.map((p) => p.detail).join('; ')} (the flow does not fit this server's results)`);
    }
    if (ex.problems.length) return false;
    if (!ex.calls.length) continue;

    const t0 = Date.now();
    const results = ex.calls.length === 1 ? [s.callTool(ex.calls[0].name, ex.calls[0].args)] : s.callParallel(ex.calls.map((c) => ({ name: c.name, args: c.args })));
    stepDuration.add(Date.now() - t0, { flow: flow.name, flow_step: step.name });

    let ok = true;
    for (let j = 0; j < results.length; j++) {
      const res = results[j];
      if (!failed(res)) continue;
      ok = false;
      // isError results are counted in mcp_tool_error_rate, not logged.
      if (res && res.error && res.error.type !== 'tool_iserror') logError(`${flow.name}/${step.name}/${ex.calls[j].name}: ${res.error.type}: ${res.error.message}`);
    }
    check(results, {
      'tools/call no transport error': (rs) => rs.every((x) => !x.error || x.error.type === 'tool_iserror'),
    });
    // A failed call ends the flow: the agent could not finish the business task.
    if (!ok) return false;
    recordResults(step, ex.calls, results, scope);
  }
  return true;
}

function runFlow(flow) {
  const t0 = Date.now();
  const tag = { flow: flow.name };
  let s;
  try {
    s = client.connect();
  } catch (e) {
    logError(`connect failed: ${e}`);
    check(null, { 'connect ok': () => false });
    flowComplete.add(false, tag);
    // Back off so a dead/unauthorised target is not hammered in a hot loop.
    backoff();
    return;
  }
  check(s, { 'connect ok': () => true });

  let complete = false;
  try {
    const listed = s.listTools();
    if (!check(listed, { 'tools/list returned tools': (l) => Array.isArray(l) && l.length > 0 })) return;
    const names = listed.map((t) => t && t.name);
    const absent = missingTools(flow, names);
    check(null, { 'workload tools listed': () => absent.length === 0 });
    if (absent.length) {
      if (!warnedMissing[flow.name]) {
        warnedMissing[flow.name] = true;
        console.warn(`[vu ${__VU}] flow '${flow.name}' calls tools the server does not list: ${absent.join(', ')}; server lists: ${names.join(', ') || '(none)'}`);
      }
      backoff();
      return;
    }
    const steps = renderSteps(flow.steps, pickRows(DATA, exec.scenario.iterationInTest, Math.random));
    complete = runSteps(s, flow, steps, baseArgsFn(listed));
    if (complete) flowDuration.add(Date.now() - t0, tag);
  } catch (e) {
    logError(`flow '${flow.name}' error: ${e}`);
  } finally {
    flowComplete.add(complete, tag);
    try {
      s.close();
    } catch (e) {
      logError(`close failed: ${e}`);
    }
  }
}

/** One agent session: pick a flow by weight and run it. Every metric of the session carries the flow tag. */
export function runWorkloadSession() {
  const flow = pickFlow(WORKLOAD.flows, Math.random());
  const tags = exec.vu.metrics.tags;
  tags.flow = flow.name;
  try {
    runFlow(flow);
  } finally {
    delete tags.flow;
  }
}

export default function () {
  runWorkloadSession();
}
