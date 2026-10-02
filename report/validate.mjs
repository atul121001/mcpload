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
