#!/usr/bin/env node
// Validate one or more mcpload report.json files against report/schema/report.v1.json,
// plus semantic checks JSON Schema cannot express (parallel array lengths, phase order, sums).
// cmd/mcpload/internal/report.(*Report).Check implements the same rules in Go (`mcpload validate`).
//
// Usage: node report/validate.mjs <report.json> [more.json ...]
// Exit code: 0 all valid, 1 any invalid, 2 usage error.
import { readFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';

const here = dirname(fileURLToPath(import.meta.url));
const schema = JSON.parse(readFileSync(join(here, 'schema', 'report.v1.json'), 'utf8'));

const ajv = new Ajv2020({ allErrors: true, strict: true, strictRequired: false });
addFormats(ajv);
const validateSchema = ajv.compile(schema);

/** Semantic checks beyond JSON Schema. Returns a list of error strings. */
export function semanticErrors(r) {
  const errs = [];
  const { phases: p, series: s, summary } = r;
  if (!(p.warmupEndS <= p.loadEndS && p.loadEndS <= p.cooldownEndS)) {
    errs.push(`phases must satisfy warmupEndS <= loadEndS <= cooldownEndS (got ${p.warmupEndS}, ${p.loadEndS}, ${p.cooldownEndS})`);
  }
  if (Date.parse(r.run.endedAt) < Date.parse(r.run.startedAt)) errs.push('run.endedAt is before run.startedAt');
  const n = s.t.length;
  for (let i = 1; i < n; i++) {
    if (!(s.t[i] > s.t[i - 1])) { errs.push(`series.t must be strictly increasing (index ${i})`); break; }
  }
  const check = (path, arr, allowEmpty = false) => {
    if (arr === undefined) return;
    if (allowEmpty && arr.length === 0) return;
    if (arr.length !== n) errs.push(`series.${path} has length ${arr.length}, expected ${n} (length of series.t)`);
  };
  for (const k of ['p95Ms', 'errorRate', 'rps']) check(`client.${k}`, s.client[k]);
  check('client.droppedIterations', s.client.droppedIterations, true);
  for (const [name, ts] of Object.entries(s.tools || {}).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))) check(`tools[${name}].p95Ms`, ts.p95Ms);
  const noSampler = s.server.sampler === 'none';
  for (const k of ['rssBytes', 'heapBytes', 'openFds', 'activeSessions']) check(`server.${k}`, s.server[k], noSampler);
  if (!noSampler && s.server.rssBytes.length === 0) errs.push(`series.server.rssBytes is empty but sampler is '${s.server.sampler}'`);
  if (summary.errors > summary.reqs) errs.push('summary.errors > summary.reqs');
  const byType = Object.values(summary.byErrorType).reduce((a, b) => a + b, 0);
  if (byType !== summary.errors) errs.push(`sum of summary.byErrorType (${byType}) != summary.errors (${summary.errors})`);
  const names = new Set();
  for (const t of r.tools) {
    if (names.has(t.name)) errs.push(`duplicate tool '${t.name}'`);
    names.add(t.name);
    if (t.errors > t.reqs) errs.push(`tools[${t.name}].errors > reqs`);
    if (!(t.p50 <= t.p95 && t.p95 <= t.p99 && t.p99 <= t.max)) errs.push(`tools[${t.name}] percentiles not monotonic (p50<=p95<=p99<=max)`);
  }
  const mono = (path, l) => {
    if (!(l.p50 <= l.p95 && l.p95 <= l.p99 && l.p99 <= l.max)) errs.push(`${path} percentiles not monotonic (p50<=p95<=p99<=max)`);
  };
  const w = r.workflow;
  if (w) {
    if (w.completed > w.runs) errs.push('workflow.completed > workflow.runs');
    mono('workflow.durationMs', w.durationMs);
    const steps = new Set();
    for (const st of w.steps) {
      if (steps.has(st.name)) errs.push(`duplicate workflow step '${st.name}'`);
      steps.add(st.name);
      mono(`workflow.steps[${st.name}]`, st);
    }
  }
  if (r.workload) {
    const flows = new Set();
    for (const f of r.workload.flows) {
      const p = `workload.flows[${f.name}]`;
      if (flows.has(f.name)) errs.push(`duplicate workload flow '${f.name}'`);
      flows.add(f.name);
      if (f.completed > f.runs) errs.push(`${p}.completed > runs`);
      mono(`${p}.durationMs`, f.durationMs);
      const steps = new Set();
      for (const st of f.steps) {
        if (steps.has(st.name)) errs.push(`duplicate step '${st.name}' in ${p}`);
        steps.add(st.name);
        mono(`${p}.steps[${st.name}]`, st);
      }
    }
  }
  const c = r.capacity;
  if (c) {
    const pv = c.plannedVus || [];
    for (let i = 1; i < pv.length; i++) {
      if (!(pv[i] > pv[i - 1])) { errs.push('capacity.plannedVus must be increasing integers >= 1'); break; }
    }
    c.steps.forEach((st, i) => {
      const path = `capacity.steps[${i}]`;
      if (i > 0 && !(st.vus > c.steps[i - 1].vus)) errs.push(`capacity.steps must be sorted by strictly increasing vus (index ${i})`);
      if (st.endS < st.startS) errs.push(`${path}.endS is before startS`);
      if (st.errors > st.reqs) errs.push(`${path}: errors must be in [0, reqs]`);
      for (const t of st.tools) {
        if (t.errors > t.reqs) errs.push(`${path}.tools[${t.name}]: errors must be in [0, reqs]`);
        if (t.p95 != null && t.p99 != null && t.p95 > t.p99) errs.push(`${path}.tools[${t.name}] percentiles not monotonic (p95<=p99)`);
      }
    });
  }
  const ss = r.sessions;
  if (ss) {
    if (ss.survived + ss.died !== ss.total) errs.push(`sessions.survived + sessions.died (${ss.survived + ss.died}) != sessions.total (${ss.total})`);
    const byCause = Object.values(ss.diedByCause).reduce((a, b) => a + b, 0);
    if (byCause !== ss.died) errs.push(`sum of sessions.diedByCause (${byCause}) != sessions.died (${ss.died})`);
  }
  const rc = r.chaos && r.chaos.recovery;
  if (rc) {
    if (rc.recovered !== (rc.recoveryS !== null)) errs.push('chaos.recovery.recoveryS must be set exactly when recovered is true');
    if (rc.connectFailures > rc.connectAttempts) errs.push('chaos.recovery: counts must be >= 0 and connectFailures <= connectAttempts');
  }
  const ci = r.callIntegrity;
  if (ci) {
    if (ci.failedButExecuted + ci.neverRan !== ci.clientFailed) errs.push(`callIntegrity.failedButExecuted + neverRan (${ci.failedButExecuted + ci.neverRan}) != clientFailed (${ci.clientFailed})`);
    if (ci.executed > ci.executions || ci.duplicated > ci.executed || ci.duplicatedAfterRetry > ci.duplicated) {
      errs.push('callIntegrity: executed <= executions, duplicated <= executed and duplicatedAfterRetry <= duplicated must hold');
    }
  }
  const cx = r.cancellation;
  if (cx) {
    if (cx.lateResponses > cx.cancels) errs.push('cancellation: lateResponses must be in [0, cancels]');
    for (const k of ['sendMs', 'lateAfterMs']) {
      const l = cx[k];
      if (l && !(l.p50 <= l.p95 && l.p95 <= l.p99 && l.p99 <= l.max)) errs.push(`cancellation.${k} percentiles not monotonic (p50<=p95<=p99<=max)`);
    }
    const sv = cx.server;
    if (sv && sv.workAfterCancelP50Ms != null && sv.workAfterCancelP95Ms != null && sv.workAfterCancelP50Ms > sv.workAfterCancelP95Ms) {
      errs.push('cancellation.server work-after-cancel percentiles not monotonic (p50<=p95)');
    }
  }
  const cmp = r.comparison;
  if (cmp) {
    let regressed = false;
    const seen = new Set();
    for (const t of cmp.tools) {
      const tp = `comparison.tools[${t.name}]`;
      if (seen.has(t.name)) errs.push(`duplicate comparison tool '${t.name}'`);
      seen.add(t.name);
      if (t.status === 'added' && (t.base !== null || t.current === null)) errs.push(`${tp}: an added tool has current and no base`);
      else if (t.status === 'removed' && (t.base === null || t.current !== null)) errs.push(`${tp}: a removed tool has base and no current`);
      else if (t.status !== 'added' && t.status !== 'removed' && (t.base === null || t.current === null)) errs.push(`${tp}.base and .current are required for status "${t.status}"`);
      regressed = regressed || t.status === 'regressed';
      for (const s of [t.base, t.current]) {
        if (!s) continue;
        if (s.errors > s.reqs) errs.push(`${tp}: errors must be in [0, reqs]`);
        if (!(s.p50 <= s.p95 && s.p95 <= s.p99)) errs.push(`${tp} percentiles not monotonic (p50<=p95<=p99)`);
      }
    }
    for (const m of cmp.metrics) regressed = regressed || m.status === 'regressed';
    if (regressed !== cmp.regressed) errs.push('comparison.regressed must be true exactly when a tool or metric has status "regressed"');
  }
  return errs;
}

export function validateReport(report) {
  if (!validateSchema(report)) {
    return validateSchema.errors.map((e) => `${e.instancePath || '/'} ${e.message}${e.params && Object.keys(e.params).length ? ' ' + JSON.stringify(e.params) : ''}`);
  }
  return semanticErrors(report);
}

const isMain = process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const files = process.argv.slice(2);
  if (files.length === 0) {
    console.error('usage: node validate.mjs <report.json> [more.json ...]');
    process.exit(2);
  }
  let bad = 0;
  for (const f of files) {
    let report;
    try {
      report = JSON.parse(readFileSync(f, 'utf8'));
    } catch (e) {
      console.error(`FAIL ${f}: cannot read/parse JSON: ${e.message}`);
      bad++;
      continue;
    }
    const errs = validateReport(report);
    if (errs.length) {
      bad++;
      console.error(`FAIL ${f}`);
      for (const e of errs) console.error(`  - ${e}`);
    } else {
      console.log(`ok   ${f} (schemaVersion ${report.schemaVersion}, ${report.series.t.length} samples, ${report.verdicts.length} verdicts)`);
    }
  }
  process.exit(bad ? 1 : 0);
}
