// Renders the Homebrew formula from the template and a release's checksums.txt.
//
//   node packaging/homebrew/render.mjs <version> <checksums.txt> [out.rb]
//
// <version> may be given with or without the leading "v" (v0.3.0 or 0.3.0).
// Without [out.rb] the formula is written to stdout.
import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const TARGETS = ['darwin_arm64', 'darwin_amd64', 'linux_arm64', 'linux_amd64'];

export function render(template, version, checksums) {
  const v = version.replace(/^v/, '');
  if (!/^\d+\.\d+\.\d+([-+][0-9A-Za-z.+-]+)?$/.test(v)) {
    throw new Error(`invalid version ${JSON.stringify(version)}`);
  }
  const sums = new Map();
  for (const line of checksums.split(/\r?\n/)) {
    const m = line.trim().match(/^([0-9a-f]{64})\s+\*?(\S+)$/);
    if (m) sums.set(m[2], m[1]);
  }
  const values = { VERSION: v };
  for (const t of TARGETS) {
    const file = `mcpload_${v}_${t}.tar.gz`;
    const sum = sums.get(file);
    if (!sum) throw new Error(`checksums.txt has no entry for ${file}`);
    values[`SHA256_${t.toUpperCase()}`] = sum;
  }
  const out = template.replace(/\{\{([A-Z0-9_]+)\}\}/g, (all, key) => {
    if (!(key in values)) throw new Error(`unknown placeholder ${all}`);
    return values[key];
  });
  return out;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const [version, checksumsPath, outPath] = process.argv.slice(2);
  if (!version || !checksumsPath) {
    console.error('usage: node packaging/homebrew/render.mjs <version> <checksums.txt> [out.rb]');
    process.exit(2);
  }
  try {
    const here = dirname(fileURLToPath(import.meta.url));
    const template = readFileSync(join(here, 'mcpload.rb.tmpl'), 'utf8');
    const formula = render(template, version, readFileSync(checksumsPath, 'utf8'));
    if (outPath) writeFileSync(outPath, formula);
    else process.stdout.write(formula);
  } catch (err) {
    console.error(`render: ${err.message}`);
    process.exit(1);
  }
}
