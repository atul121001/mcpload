// Tests for action/summary.mjs: node action/summary.test.mjs (no dependencies).
//
// testdata/comparison.json is a report.comparison written by `mcpload compare
// --format json` and testdata/comparison.md the Markdown the CLI renders for it
// (cmd/mcpload/internal/cli TestCompareGolden keeps both current), so the PR
// comment and `mcpload compare --format markdown` stay the same.
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { renderComparison, renderMarkdown } from './summary.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const read = (p) => readFileSync(join(here, p), 'utf8').replace(/\r\n/g, '\n');
const comparison = JSON.parse(read('testdata/comparison.json'));

// Same Markdown as the CLI.
assert.equal(renderComparison(comparison).join('\n'), read('testdata/comparison.md'));

// In the summary: after "Why it failed", before the per-tool table.
const report = JSON.parse(read('../report/examples/healthy.json'));
report.comparison = comparison;
report.verdicts.push({ id: 'regression', status: 'fail', signal: 'comparison', message: 'Performance regression vs baseline 4a1b2c3 (main): `search` p95 372 ms → 500 ms (+34%, +128 ms).' });
const md = renderMarkdown(report);
const at = (s) => { const i = md.indexOf(s); assert.ok(i >= 0, `missing ${s}`); return i; };
assert.ok(at('### Why it failed') < at('### Compared with baseline'));
assert.ok(at('### Compared with baseline') < at('### Per-tool latency and errors'));
assert.ok(md.includes('- **regression**: Performance regression vs baseline 4a1b2c3 (main)'));

// Warnings and notes.
const warned = { ...comparison, regressed: false, reasons: [], warnings: ['scenario differs (baseline soak, current burst); the comparison may be unfair'],
  tools: [], metrics: [{ id: 'memoryGrowthMiB', label: 'memory growth', unit: 'MiB', base: 1.1, current: 8.2, status: 'n/a', note: 'not judged: needs 2+ min of load after the first 60 s' }] };
const wmd = renderComparison(warned).join('\n');
assert.ok(wmd.includes('**No regression:**'));
assert.ok(wmd.includes('> ⚠ scenario differs (baseline soak, current burst); the comparison may be unfair'));
assert.ok(wmd.includes('| memory growth | 1.1 MiB | 8.2 MiB | +7.1 MiB (+645%) | not judged: needs 2+ min of load after the first 60 s |'));

// A stdio run names the server command instead of a url.
const stdio = JSON.parse(read('../report/examples/healthy.json'));
stdio.run.target = { command: ['node', 'server.mjs', '--port', '0'], transport: 'stdio' };
const smd = renderMarkdown(stdio);
assert.ok(smd.includes('— node server.mjs --port 0'), smd.split('\n')[0]);
assert.ok(smd.includes('**Target** stdio `node server.mjs --port 0`'));

// The script writes regressed=true|false for the action output.
const dir = mkdtempSync(join(tmpdir(), 'mcpload-summary-'));
writeFileSync(join(dir, 'r.json'), JSON.stringify(report));
const run = (file) => {
  execFileSync(process.execPath, [join(here, 'summary.mjs'), file, '--out', join(dir, 'out.md'), '--github-output', join(dir, 'out.txt')], { stdio: 'ignore' });
  return readFileSync(join(dir, 'out.txt'), 'utf8');
};
assert.equal(run(join(dir, 'r.json')), 'result=fail\nregressed=true\n');
writeFileSync(join(dir, 'out.txt'), '');
assert.equal(run(join(here, '..', 'report', 'examples', 'healthy.json')), 'result=pass\nregressed=false\n');

console.log('summary.test.mjs: ok');
