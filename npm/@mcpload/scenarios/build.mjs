// build.mjs — Copy scenarios from the main repo into this package for publishing.
// Run: node build.mjs
//
// This is called by prepublishOnly in package.json.
// When developing locally, you can also just symlink or run k6 directly
// from the main repo's scenarios/ folder.

import { copyFileSync, mkdirSync, readdirSync, rmSync } from 'node:fs';
import { join, resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(__dirname, '..', '..', '..'); // Up to mcp-load-test root (3 levels from npm/@mcpload/scenarios/)
const SCENARIOS_SRC = join(ROOT, 'scenarios');
const PKG_DST = __dirname;

function copyScenarios() {
  console.log('Copying scenarios from', SCENARIOS_SRC);
  console.log('to', PKG_DST);

  // Copy top-level .js files (not .test.mjs or package.json)
  for (const entry of readdirSync(SCENARIOS_SRC, { withFileTypes: true })) {
    if (entry.isDirectory()) continue;
    if (!entry.name.endsWith('.js')) continue;
    const src = join(SCENARIOS_SRC, entry.name);
    const dst = join(PKG_DST, entry.name);
    copyFileSync(src, dst);
    console.log(`  ${entry.name}`);
  }

  // Copy lib/ contents
  const libSrc = join(SCENARIOS_SRC, 'lib');
  const libDst = join(PKG_DST, 'lib');
  mkdirSync(libDst, { recursive: true });

  for (const entry of readdirSync(libSrc, { withFileTypes: true })) {
    if (entry.isDirectory()) continue;
    if (!entry.name.endsWith('.js')) continue; // Skip .test.mjs files
    const src = join(libSrc, entry.name);
    const dst = join(libDst, entry.name);
    copyFileSync(src, dst);
    console.log(`  lib/${entry.name}`);
  }

  console.log('Done.');
}

function clean() {
  console.log('Cleaning copied files...');
  // Remove copied .js files (not package.json, README.md, build.mjs)
  for (const entry of readdirSync(PKG_DST, { withFileTypes: true })) {
    if (entry.name.endsWith('.js') || entry.name === 'lib') {
      if (entry.isDirectory()) {
        rmSync(join(PKG_DST, entry.name), { recursive: true, force: true });
      } else {
        rmSync(join(PKG_DST, entry.name));
      }
    }
  }
  console.log('Cleaned.');
}

const action = process.argv[2] || 'build';
if (action === 'clean') {
  clean();
} else {
  copyScenarios();
}
