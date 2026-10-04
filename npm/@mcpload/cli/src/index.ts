#!/usr/bin/env node
/**
 * mcpload — CLI wrapper for mcpload Go binary
 * Downloads the Go binary from GitHub releases, verifies it, and runs it.
 */
import { spawn } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { detectPlatform, downloadBinary, latestRelease, type Platform } from './downloader.js';
import { writeCache, cachedVersion } from './cache.js';

// Determine package version from package.json
const __dirname = dirname(fileURLToPath(import.meta.url));
const pkg = JSON.parse(readFileSync(resolve(__dirname, '..', 'package.json'), 'utf-8'));
const PACKAGE_VERSION = pkg.version as string;

const log = (msg: string) => process.stderr.write(`mcpload: ${msg}\n`);

async function main(): Promise<void> {
  // --update / --force-update are for this wrapper only; mcpload itself would
  // reject them as unknown flags.
  const raw = process.argv.slice(2);
  const wantsUpdate = raw.includes('--update') || raw.includes('--force-update');
  const args = raw.filter((a) => a !== '--update' && a !== '--force-update');
  const platform = detectPlatform();

  // Determine target version
  let targetVersion: string;
  const envVersion = process.env.MCPLOAD_VERSION;

  if (wantsUpdate) {
    log('Updating to latest release...');
    targetVersion = await latestRelease();
  } else if (envVersion) {
    targetVersion = envVersion.startsWith('v') ? envVersion : `v${envVersion}`;
  } else {
    // Pin to npm package version by default (e.g., 0.4.1 -> v0.4.1)
    targetVersion = `v${PACKAGE_VERSION}`;
  }

  // Ensure binary is downloaded
  const binPath = await ensureBinary(targetVersion, platform);

  // Update cache if version changed
  const currentCached = cachedVersion();
  if (currentCached !== targetVersion) {
    writeCache({ version: targetVersion, checkedAt: new Date().toISOString() });
  }

  // Run mcpload with all arguments forwarded
  const child = spawn(binPath, args, {
    stdio: ['inherit', 'inherit', 'inherit'],
    env: { ...process.env },
    cwd: process.cwd(),
  });

  child.on('error', (err) => {
    log(`Failed to start mcpload: ${err.message}`);
    process.exit(1);
  });

  child.on('close', (code) => {
    process.exit(code ?? 1);
  });
}

async function ensureBinary(version: string, platform: Platform): Promise<string> {
  return downloadBinary(version, platform, (msg) => log(msg));
}

main().catch((err) => {
  log(`Error: ${err.message}`);
  process.exit(1);
});