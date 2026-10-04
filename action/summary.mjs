#!/usr/bin/env node
// Turn an mcpload report.json into a Markdown summary (GitHub step summary / PR comment).
//
//   node action/summary.mjs <report.json> [--out file.md] [--append-to file.md]
//        [--run-url URL] [--artifact-url URL] [--exit-code N] [--marker TEXT] [--title TEXT]
//        [--github-output file]
//
// No dependencies (runs on the stock Node of GitHub runners, Node >= 18).
// The overall result follows report/schema/README.md "Overall result":
//   fail if any verdicts[].status is "fail" or any thresholds[].passed is false,
//   otherwise pass (with warnings if any verdict is "warn").
// If the report is missing or unreadable, the summary says so and the result is "error".
import { appendFileSync, existsSync, readFileSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

function parseArgs(argv) {
  const opts = { positional: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a.startsWith('--')) {
      const key = a.slice(2);
      const next = argv[i + 1];
      if (next === undefined || next.startsWith('--')) opts[key] = true;
      else { opts[key] = next; i++; }
    } else opts.positional.push(a);
  }
  return opts;
}

const fmtMs = (v) => (v === null || v === undefined || !Number.isFinite(v) ? '–' : v >= 1000 ? `${(v / 1000).toFixed(2)} s` : `${Math.round(v)} ms`);
const fmtPct = (v) => (v === null || v === undefined || !Number.isFinite(v) ? '–' : `${(v * 100).toFixed(v > 0 && v < 0.001 ? 3 : 2)}%`);
const fmtInt = (v) => (Number.isFinite(v) ? v.toLocaleString('en-US') : '–');
const fmtDur = (s) => {
  if (!Number.isFinite(s)) return '–';
  // Round once, up front, so 119.6 s reads "2m" rather than "1m 60s".
  const total = Math.max(0, Math.round(s));
  const m = Math.floor(total / 60), r = total % 60;
  return m ? `${m}m${r ? ` ${r}s` : ''}` : `${r}s`;
};
// Sanitize a value for inclusion in the Markdown PR comment. Values are
// server-controlled (tool names, verdict messages, URLs) so we must stop them
// breaking out of a code span (backtick), a table cell (pipe), or injecting
// links, images or @mentions. Control characters become spaces so a newline
// can't split a table row. Backticks, pipes, HTML angle brackets and @ are
// dropped. Bold/italic markers (* and _) are left alone: they are harmless
// and escaping would turn `memory_leak` into `memory\_leak`. Kept in sync
// with mdSanitize in cmd/mcpload/internal/cli/compare.go. CodeQL #3; L4.
const cell = (s) => String(s ?? '')
  .replace(/[\x00-\x1f\x7f]/g, ' ')
  .replace(/[`<>|@]/g, '')
  .replace(/[[\]!]/g, '\\$&');

const STATUS_LABEL = { pass: 'PASS', warn: 'WARN', fail: 'FAIL', skipped: 'SKIP' };
const STATUS_RANK = { fail: 0, warn: 1, pass: 2, skipped: 3 };

export function overall(report) {
  const verdicts = Array.isArray(report.verdicts) ? report.verdicts : [];
  const thresholds = Array.isArray(report.thresholds) ? report.thresholds : [];
  const failedVerdicts = verdicts.filter((v) => v.status === 'fail');
  const failedThresholds = thresholds.filter((t) => t.passed === false);
  // Unknown statuses count as warn (schema README, "Versioning").
  const warned = verdicts.filter((v) => v.status !== 'fail' && v.status !== 'pass' && v.status !== 'skipped');
  const passed = failedVerdicts.length === 0 && failedThresholds.length === 0;
  return { passed, warnings: warned.length, failedVerdicts, failedThresholds };
}

// Per-tool budget status from thresholds whose metric selector names the tool.
function toolBudget(report, tool) {
  const ts = (report.thresholds || []).filter((t) => typeof t.metric === 'string' && t.metric.includes(`{tool:${tool}}`));
  if (!ts.length) return '–';
  const bad = ts.filter((t) => t.passed === false);
  return bad.length ? `**FAIL** (${bad.map((t) => cell(t.expr)).join(', ')})` : 'pass';
}

// ---------- comparison with a baseline (report.comparison, `mcpload run --baseline`) ----------
// Same layout as `mcpload compare --format markdown` (cmd/mcpload/internal/cli/compare.go).
const MARK = { regressed: '⚠', improved: '✓', ok: '·' };
const trim0 = (s) => s.replace(/\.?0+$/, '');
const cmpMs = (v) => (v >= 1000 ? `${trim0(trim0((v / 1000).toFixed(2)))} s` : v < 10 ? `${v.toFixed(1)} ms` : `${Math.round(v)} ms`);
const cmpVal = (unit, v) => {
  if (v === null || v === undefined || !Number.isFinite(v)) return '–';
  switch (unit) {
    case 'ms': return cmpMs(v);
    case 'rate': return `${trim0((v * 100).toFixed(2))}%`;
    case 'req/s': return v.toFixed(1);
    case 'MiB': return `${v.toFixed(1)} MiB`;
    case 'MiB/min': return `${v.toFixed(2)} MiB/min`;
    default: return `${v} ${unit}`;
  }
};
const cmpPct = (b, c) => {
  if (!Number.isFinite(b) || !Number.isFinite(c) || b < 0) return '';
  if (b === 0) return c === 0 ? '0%' : 'new';
  const p = Math.round((100 * (c - b)) / b);
  return p === 0 ? '0%' : `${p > 0 ? '+' : ''}${p}%`;
};
const cmpDelta = (unit, b, c) => {
  if (!Number.isFinite(b) || !Number.isFinite(c)) return '';
  let d = c - b;
  const sign = d < 0 ? '-' : '+';
  d = Math.abs(d);
  const zero = { rate: d < 0.00005, ms: d < 0.05, MiB: d < 0.05, 'MiB/min': d < 0.005 }[unit] ?? d === 0;
  if (zero) return unit === 'rate' ? '0 pts' : `0 ${unit}`;
  if (unit === 'rate') return `${sign}${(d * 100).toFixed(2)} pts`;
  if (unit === 'ms') return sign + cmpMs(d);
  return sign + cmpVal(unit, d);
};
const shortName = (b) => {
  if (b.git && b.git.sha) {
    const ref = String(b.git.ref || '').replace(/^refs\/heads\//, '').replace(/^refs\//, '');
    return String(b.git.sha).slice(0, 7) + (ref ? ` (${ref})` : '');
  }
  return `run ${String(b.runId || '').slice(0, 8)}`;
};

export function renderComparison(c) {
  const lines = [];
  const b = c.baseline || {};
  const ru = c.rules || {};
  const tools = Array.isArray(c.tools) ? c.tools : [];
  const metrics = Array.isArray(c.metrics) ? c.metrics : [];
  lines.push('### Compared with baseline', '');
  lines.push(`Baseline \`${cell(shortName(b))}\` · ${cell(b.scenario)} · ${cell(b.protocol)} · started ${cell(b.startedAt)}`, '');
  if (c.regressed) {
    lines.push('**Performance regression detected:**', '', ...(c.reasons || []).map((r) => `- ${cell(r)}`), '');
  } else {
    lines.push('**No regression:** every difference is within noise or an improvement.', '');
  }
  for (const w of c.warnings || []) lines.push(`> ⚠ ${cell(w)}`);
  if ((c.warnings || []).length) lines.push('');
  const field = (t, f, judged) => (!judged ? '' : (t.regressed || []).includes(f) ? 'regressed' : (t.improved || []).includes(f) ? 'improved' : 'ok');
  const mdCell = (unit, bv, cv, st) => {
    if (!Number.isFinite(bv)) return `– → ${cmpVal(unit, cv)}`;
    if (!Number.isFinite(cv)) return `${cmpVal(unit, bv)} → –`;
    let s = `${cmpVal(unit, bv)} → ${cmpVal(unit, cv)}`;
    const p = cmpPct(bv, cv);
    if (p && p !== '0%' && unit !== 'req/s') s += ` (${p})`;
    if (MARK[st]) s += ` ${MARK[st]}`;
    return s;
  };
  if (tools.length) {
    lines.push('| Tool | Calls | p50 | p95 | p99 | Error rate | req/s |', '|---|--:|--:|--:|--:|--:|--:|');
    for (const t of tools) {
      const tb = t.base || {}, tc = t.current || {};
      const both = !!(t.base && t.current) && t.status !== 'few_calls';
      const lat = both && tb.reqs - tb.errors >= ru.minCalls && tc.reqs - tc.errors >= ru.minCalls;
      const tag = { added: ' (added)', removed: ' (removed)', few_calls: ' (too few calls)' }[t.status] || '';
      const calls = `${t.base ? fmtInt(tb.reqs) : '–'} → ${t.current ? fmtInt(tc.reqs) : '–'}`;
      lines.push(`| \`${cell(t.name)}\`${tag} | ${calls} | ${mdCell('ms', tb.p50, tc.p50, '')} | ${mdCell('ms', tb.p95, tc.p95, field(t, 'p95', lat))} | ` +
        `${mdCell('ms', tb.p99, tc.p99, field(t, 'p99', lat))} | ${mdCell('rate', tb.errorRate, tc.errorRate, field(t, 'errorRate', both))} | ${mdCell('req/s', tb.rps, tc.rps, '')} |`);
    }
    lines.push('');
  }
  if (metrics.length) {
    lines.push('| Run | Baseline | Current | Δ | |', '|---|--:|--:|--:|---|');
    for (const m of metrics) {
      let d = cmpDelta(m.unit, m.base, m.current);
      const p = cmpPct(m.base, m.current);
      if (d && p && p !== '0%') d += ` (${p})`;
      const mark = [MARK[m.status] || '', m.note || ''].filter(Boolean).join(' ');
      lines.push(`| ${cell(m.label)} | ${cmpVal(m.unit, m.base)} | ${cmpVal(m.unit, m.current)} | ${d} | ${cell(mark)} |`);
    }
    lines.push('');
  }
  const n = (v) => +(v * 100).toFixed(6);
  lines.push(`<sub>Rules: p95 +${n(ru.maxP95Increase)}% / p99 +${n(ru.maxP99Increase)}% and +${ru.minDeltaMs} ms; error rate +${n(ru.minErrorDelta)} pts, +${n(ru.maxErrorIncrease)}% and p < 0.001; at least ${ru.minCalls} calls per tool. ⚠ regression · ✓ improvement · · within noise</sub>`, '');
  return lines;
}

export function renderMarkdown(report, o = {}) {
  const lines = [];
  const marker = o.marker ? `<!-- ${o.marker} -->` : '';
  if (marker) lines.push(marker);

  if (!report) {
    lines.push(`## ${o.title || 'mcpload'}: ERROR`, '');
    lines.push(`mcpload did not produce a report${o.exitCode !== undefined ? ` (exit code ${o.exitCode})` : ''}. Check the job log for the error.`);
    if (o.error) lines.push('', '```', String(o.error).slice(0, 2000), '```');
    return lines.join('\n') + '\n';
  }

  const res = overall(report);
  const run = report.run || {};
  const target = run.target || {};
  const name = target.label || target.url || 'MCP server';
  const verdictWord = res.passed ? (res.warnings ? 'PASS (with warnings)' : 'PASS') : 'FAIL';
  lines.push(`## ${o.title || 'mcpload'}: ${verdictWord} — ${cell(name)}`, '');

  const facts = [
    `**Target** \`${cell(target.url || '?')}\``,
    `**Scenario** \`${cell(run.scenario || '?')}\``,
    `**Protocol** \`${cell(run.protocol || '?')}\``,
    `**Duration** ${fmtDur(run.durationS)}`,
  ];
  if (run.load) {
    const l = run.load;
    const shape = l.arrivalRate ? `${l.arrivalRate}/${l.arrivalTimeUnitS || 1}s arrivals (max ${l.maxVus ?? l.vus ?? '?'} VUs)` : `${l.vus ?? '?'} VUs`;
    facts.push(`**Load** ${cell(l.executor || '')} ${shape}`);
  }
  if (run.git && run.git.sha) facts.push(`**Commit** \`${String(run.git.sha).slice(0, 12)}\``);
  lines.push(facts.join(' · '), '');

  const s = report.summary || {};
  const byType = Object.entries(s.byErrorType || {}).filter(([, n]) => n > 0);
  const typeText = byType.length ? ` · by type: ${byType.map(([k, n]) => `\`${cell(k)}\` ${fmtInt(n)}`).join(', ')}` : '';
  lines.push(`**Requests** ${fmtInt(s.reqs)} · **Errors** ${fmtInt(s.errors)} (${fmtPct(s.errorRate)})${typeText}`, '');

  // Load generator health (optional fields; older reports don't have them).
  const gen = run.generator && typeof run.generator === 'object' ? run.generator : null;
  const genFacts = [];
  if (Number.isFinite(s.iterations)) genFacts.push(`**Iterations** ${fmtInt(s.iterations)}`);
  if (Number.isFinite(s.droppedIterations)) {
    const planned = Number.isFinite(s.iterations) ? s.iterations + s.droppedIterations : NaN;
    const share = planned > 0 ? ` (${fmtPct(s.droppedIterations / planned)} of planned)` : '';
    genFacts.push(`**Dropped iterations** ${fmtInt(s.droppedIterations)}${share}`);
  }
  if (gen) {
    const cpu = [];
    if (Number.isFinite(gen.cpuAvgPct)) cpu.push(`avg ${gen.cpuAvgPct.toFixed(0)}%`);
    if (Number.isFinite(gen.cpuMaxPct)) cpu.push(`max ${gen.cpuMaxPct.toFixed(0)}%`);
    const cores = Number.isFinite(gen.cores) ? `${gen.cores} core${gen.cores === 1 ? '' : 's'}` : '';
    if (cpu.length) genFacts.push(`**k6 CPU** ${cpu.join(', ')}${cores ? ` on ${cores}` : ''}`);
    else if (cores) genFacts.push(`**Generator** ${cores}`);
  }
  if (genFacts.length) lines.push(genFacts.join(' · '), '');
  const genVerdict = (Array.isArray(report.verdicts) ? report.verdicts : []).find((v) => v && v.id === 'generator');
  if (genVerdict && (genVerdict.status === 'warn' || genVerdict.status === 'fail')) {
    lines.push(`> **Load generator ${genVerdict.status === 'fail' ? 'failed' : 'warning'}:** ${cell(genVerdict.message || 'k6 could not keep up with the planned load').replace(/\.\s*$/, '')}. Latency figures may include load-generator overhead; lower the load or run k6 on a separate machine.`, '');
  }

  if (!res.passed) {
    const reasons = [
      // The `threshold` verdict is listed through the individual failed thresholds below.
      ...res.failedVerdicts.filter((v) => !(v.id === 'threshold' && res.failedThresholds.length)).map((v) => `- **${cell(v.id)}**: ${cell(v.message || 'failed')}`),
      ...res.failedThresholds.map((t) => `- threshold \`${cell(t.metric)}\` \`${cell(t.expr)}\` failed (observed ${t.observed ?? 'no samples'})`),
    ];
    lines.push('### Why it failed', '', ...reasons.slice(0, 20));
    if (reasons.length > 20) lines.push(`- … and ${reasons.length - 20} more`);
    lines.push('');
  }

  if (report.comparison && typeof report.comparison === 'object') lines.push(...renderComparison(report.comparison));

  const tools = Array.isArray(report.tools) ? report.tools : [];
  if (tools.length) {
    lines.push('### Per-tool latency and errors', '');
    lines.push('| Tool | Requests | p50 | p95 | p99 | max | Error rate | Budget |');
    lines.push('|---|--:|--:|--:|--:|--:|--:|---|');
    for (const t of tools) {
      lines.push(`| \`${cell(t.name)}\` | ${fmtInt(t.reqs)} | ${fmtMs(t.p50)} | ${fmtMs(t.p95)} | ${fmtMs(t.p99)} | ${fmtMs(t.max)} | ${fmtPct(t.errorRate)} | ${toolBudget(report, t.name)} |`);
    }
    lines.push('');
  }

  const verdicts = Array.isArray(report.verdicts) ? [...report.verdicts] : [];
  if (verdicts.length) {
    verdicts.sort((a, b) => (STATUS_RANK[a.status] ?? 1) - (STATUS_RANK[b.status] ?? 1));
    lines.push('### Verdicts', '');
    lines.push('| Status | Verdict | Details |');
    lines.push('|---|---|---|');
    for (const v of verdicts) {
      const label = STATUS_LABEL[v.status] || String(v.status || '?').toUpperCase();
      const strong = v.status === 'fail' || v.status === 'warn' ? `**${label}**` : label;
      lines.push(`| ${strong} | \`${cell(v.id)}\` | ${cell(v.message || '')} |`);
    }
    lines.push('');
  }

  const links = [];
  if (o.runUrl) links.push(`[Uploaded run](${o.runUrl})`);
  if (o.artifactUrl) links.push(`[report.html / report.json artifact](${o.artifactUrl})`);
  else links.push('Full report: `report.html` in the workflow artifacts');
  lines.push(links.join(' · '));
  const sampler = report.series && report.series.server && report.series.server.sampler;
  if (sampler === 'none') lines.push('', '_Server sampler was `none`: memory/session/fd leak verdicts are skipped. Use `sampler: docker` or `prometheus` for soak runs._');
  lines.push('', `<sub>mcpload ${cell((report.tool && report.tool.version) || '')} · report schema v${cell(report.schemaVersion)} · run \`${cell(run.id || '')}\`</sub>`);
  return lines.join('\n') + '\n';
}

function main() {
  const o = parseArgs(process.argv.slice(2));
  const file = o.positional[0];
  if (!file) {
    console.error('usage: node summary.mjs <report.json> [--out f.md] [--append-to f.md] [--run-url U] [--artifact-url U] [--exit-code N] [--marker T] [--title T] [--github-output f]');
    process.exit(2);
  }
  let report = null, error;
  if (existsSync(file)) {
    try { report = JSON.parse(readFileSync(file, 'utf8')); } catch (e) { error = `could not parse ${file}: ${e.message}`; }
  } else error = `${file} not found`;

  const md = renderMarkdown(report, {
    runUrl: typeof o['run-url'] === 'string' ? o['run-url'] : '',
    artifactUrl: typeof o['artifact-url'] === 'string' ? o['artifact-url'] : '',
    exitCode: typeof o['exit-code'] === 'string' ? o['exit-code'] : undefined,
    marker: typeof o.marker === 'string' ? o.marker : '',
    title: typeof o.title === 'string' ? o.title : '',
    error,
  });

  if (typeof o.out === 'string') writeFileSync(o.out, md);
  if (typeof o['append-to'] === 'string') appendFileSync(o['append-to'], md + '\n');
  if (o.out === undefined && o['append-to'] === undefined) process.stdout.write(md);

  const result = report ? (overall(report).passed ? 'pass' : 'fail') : 'error';
  // regressed: the run was compared with a baseline (--baseline) and regressed.
  const regressed = !!(report && report.comparison && report.comparison.regressed);
  if (typeof o['github-output'] === 'string') appendFileSync(o['github-output'], `result=${result}\nregressed=${regressed}\n`);
  console.error(`mcpload summary: ${result}`);
}

// Run as a script, not when imported (action/summary.test.mjs imports it).
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) main();
