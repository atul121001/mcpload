#!/usr/bin/env node
// Render a report.json into the self-contained static HTML report.
//   node report/render.mjs report.json out.html [--no-validate]
// The JSON is embedded in <script type="application/json" id="report-data"> of template/report.html.
import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const args = process.argv.slice(2);
const noValidate = args.includes('--no-validate');
const [input, output] = args.filter((a) => !a.startsWith('--'));
if (!input || !output) {
  console.error('usage: node render.mjs <report.json> <out.html> [--no-validate]');
  process.exit(2);
}

const report = JSON.parse(readFileSync(input, 'utf8'));
if (!noValidate) {
  const { validateReport } = await import('./validate.mjs');
  const errs = validateReport(report);
  if (errs.length) {
    console.error(`invalid report ${input}:`);
    for (const e of errs) console.error(`  - ${e}`);
    process.exit(1);
  }
}

// Escape so the JSON can never terminate the <script> element or start an HTML comment.
const json = JSON.stringify(report)
  .replace(/</g, '\\u003c')
  .replace(/>/g, '\\u003e')
  .replace(/&/g, '\\u0026');

const template = readFileSync(join(here, 'template', 'report.html'), 'utf8');
const re = /(<script type="application\/json" id="report-data">)[\s\S]*?(<\/script>)/;
if (!re.test(template)) {
  console.error('template is missing <script type="application/json" id="report-data">');
  process.exit(1);
}
const label = report.run?.target?.label || report.run?.target?.url || 'report';
const title = `mcpload · ${label} · ${report.run?.scenario ?? ''}`.replace(/[<>&]/g, '');
const html = template
  .replace(re, (_, open, close) => `${open}${json}${close}`)
  .replace(/<title>[^<]*<\/title>/, `<title>${title}</title>`);
writeFileSync(output, html);
console.log(`wrote ${output} (${(html.length / 1024).toFixed(1)} KiB)`);
