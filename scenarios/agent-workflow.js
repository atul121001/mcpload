// requires k6 built with xk6-mcpload
//
// agent-workflow: VUs each run a multi-step agent plan back to back. Unlike agent-session (independent random
// rounds), the steps depend on each other: step 1 fans out parallel lookups, the agent pauses to decide, step 2
// calls tools with arguments taken from step 1's results (and may fan out once per result), and so on to a final
// call. Every step is one parallel batch, so it lasts as long as its slowest call.
//
//   connect -> tools/list -> step 1 (callParallel) -> think -> step 2 (args from step 1) -> ... -> close
//
// The plan comes from WORKFLOW (JSON, format in lib/workflow.js); the default suits the demo servers. If the
// server does not list a tool the plan needs, the VU warns, records a failed 'workflow tools listed' check and
// backs off CONNECT_BACKOFF_MS. A reference whose path is missing from a successful result (the plan does not
// fit the server) records a failed 'workflow dependency resolved' check; a reference to a failed call (e.g.
// isError) just ends that workflow early. Either way the workflow counts as not complete.
//
// Metrics (k6 custom metrics, in report.json `workflow` and the HTML report):
//   mcp_workflow_step_duration{step}  wall time of one step's parallel batch (its slowest call), ms
//   mcp_workflow_duration             one complete workflow, from connect() to the end of the last step,
//                                     think pauses included, ms
//   mcp_workflow_complete             rate of workflows that ran every step
//
//   ./k6 run -e MCP_URL=http://localhost:3001/mcp scenarios/agent-workflow.js
//   ./mcpload run --url http://localhost:3001/mcp --scenario agent-workflow
//   VUS (10), DURATION (2m), WORKFLOW (JSON plan), THINK_MS mean pause between steps (500; a step's thinkMs wins)
//   WORKFLOW_P95_MS (5000), WORKFLOW_P99_MS (unset), STEP_P95_MS (1000), STEP_P99_MS (unset),
//   STEP_BUDGETS per-step overrides as JSON, e.g. {"inspect":{"p95":600}}, WORKFLOW_MIN_COMPLETE (0.95)
import { check, sleep } from 'k6';
import { Rate, Trend } from 'k6/metrics';
import { config, buildThresholds, env, envJSON, envNum, toolBudget } from './lib/config.js';
import { makeClient } from './lib/session.js';
import { argsFromSchema } from './lib/schema-args.js';
import { DEMO_TOOL_ARGS, isDemoServer } from './lib/tools.js';
import {
  DEFAULT_WORKFLOW,
  expandStep,
  failed,
  missingTools,
  newScope,
  normalizeWorkflow,
  planToolNames,
  recordResults,
  thinkSecondsFor,
  workflowThresholds,
} from './lib/workflow.js';

const client = makeClient();
const PLAN = normalizeWorkflow(envJSON('WORKFLOW', DEFAULT_WORKFLOW));

const stepBudgets = envJSON('STEP_BUDGETS', {});
if (stepBudgets === null || typeof stepBudgets !== 'object' || Array.isArray(stepBudgets)) throw new Error('STEP_BUDGETS must be a JSON object');
const optNum = (name) => (env(name, undefined) === undefined ? undefined : envNum(name, undefined));

const stepDuration = new Trend('mcp_workflow_step_duration', true);
const workflowDuration = new Trend('mcp_workflow_duration', true);
const workflowComplete = new Rate('mcp_workflow_complete');

// Every tool the plan calls gets its own latency/error threshold (TOOL_BUDGETS, else P95_MS/P99_MS/ERR_RATE),
// so calls never need the budget:default catch-all tag.
const toolThresholds = {};
for (const name of planToolNames(PLAN)) {
  const tb = toolBudget(name);
  toolThresholds[`mcp_req_duration{tool:${name}}`] = [`p(95)<${tb.p95}`, `p(99)<${tb.p99}`];
  toolThresholds[`mcp_tool_error_rate{tool:${name}}`] = [`rate<${tb.errRate}`];
}

export const options = {
  scenarios: {
    workflows: {
      executor: 'constant-vus',
      vus: envNum('VUS', 10),
      duration: env('DURATION', '2m'),
      gracefulStop: '30s',
    },
  },
  thresholds: buildThresholds(
    Object.assign(
      toolThresholds,
      workflowThresholds(PLAN, {
        workflowP95: envNum('WORKFLOW_P95_MS', 5000),
        workflowP99: optNum('WORKFLOW_P99_MS'),
        stepP95: envNum('STEP_P95_MS', 1000),
        stepP99: optNum('STEP_P99_MS'),
        stepBudgets,
        minComplete: envNum('WORKFLOW_MIN_COMPLETE', 0.95),
      }),
    ),
  ),
  tags: { scenario_name: 'agent-workflow' },
};

export function setup() {
  const plan = PLAN.steps.map((st) => `${st.name}(${st.calls.map((c) => c.tool + (c.repeat > 1 ? `x${c.repeat}` : c.forEach ? '*' : '')).join(',')})`);
  console.log(`agent-workflow -> ${config.url} (protocol ${config.protocol}): ${plan.join(' -> ')}`);
}

let logged = 0;
function logError(msg) {
  // Keep logs readable under load: first 20 per VU only.
  if (logged++ < 20) console.warn(`[vu ${__VU}] ${msg}`);
}

let warnedMissing = false;
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

/** Run every step of the plan on an open session. Returns true when all steps ran. */
function runSteps(s, baseArgs) {
  const scope = newScope();
  for (let i = 0; i < PLAN.steps.length; i++) {
    const step = PLAN.steps[i];
    // The agent reasons about the previous step's results before deciding on the next calls.
    if (i > 0) sleep(thinkSecondsFor(step.thinkMs !== undefined ? step.thinkMs : config.thinkMs));

    const ex = expandStep(step, scope, baseArgs);
    const missing = ex.problems.filter((p) => p.reason === 'missing');
    if (ex.hasRefs && !ex.problems.some((p) => p.reason === 'failed')) {
      check(null, { 'workflow dependency resolved': () => missing.length === 0 });
    }
    if (missing.length && !warnedDeps) {
      warnedDeps = true;
      console.warn(`[vu ${__VU}] WORKFLOW step '${step.name}': ${missing.map((p) => p.detail).join('; ')} (the plan does not fit this server's results)`);
    }
    // A step whose inputs could not be built ends the workflow, as an agent missing a result would stop.
    if (ex.problems.length) return false;
    if (!ex.calls.length) continue;

    const t0 = Date.now();
    const results = ex.calls.length === 1 ? [s.callTool(ex.calls[0].name, ex.calls[0].args)] : s.callParallel(ex.calls.map((c) => ({ name: c.name, args: c.args })));
    stepDuration.add(Date.now() - t0, { step: step.name });

    for (let j = 0; j < results.length; j++) {
      const res = results[j];
      // isError results are expected (e.g. `flaky`): counted in mcp_tool_error_rate, not logged.
      if (failed(res) && res.error && res.error.type !== 'tool_iserror') logError(`${step.name}/${ex.calls[j].name}: ${res.error.type}: ${res.error.message}`);
    }
    check(results, {
      'tools/call no transport error': (rs) => rs.every((x) => !x.error || x.error.type === 'tool_iserror'),
    });
    recordResults(step, ex.calls, results, scope);
  }
  return true;
}

export default function () {
  const t0 = Date.now();
  let s;
  try {
    s = client.connect();
  } catch (e) {
    logError(`connect failed: ${e}`);
    check(null, { 'connect ok': () => false });
    workflowComplete.add(false);
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
    const absent = missingTools(PLAN, names);
    check(null, { 'workflow tools listed': () => absent.length === 0 });
    if (absent.length) {
      if (!warnedMissing) {
        warnedMissing = true;
        console.warn(`[vu ${__VU}] WORKFLOW tools not listed by the server: ${absent.join(', ')}; server lists: ${names.join(', ') || '(none)'}`);
      }
      backoff();
      return;
    }
    complete = runSteps(s, baseArgsFn(listed));
    if (complete) workflowDuration.add(Date.now() - t0);
  } catch (e) {
    logError(`workflow error: ${e}`);
  } finally {
    workflowComplete.add(complete);
    try {
      s.close();
    } catch (e) {
      logError(`close failed: ${e}`);
    }
  }
}
