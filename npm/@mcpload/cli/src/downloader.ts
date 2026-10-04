/**
 * Download, verify, and cache the mcpload Go binary from GitHub releases.
 */
import { existsSync, mkdirSync, writeFileSync, copyFileSync, chmodSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createHash } from 'node:crypto';
import { extract } from 'tar';
import { rm } from 'node:fs/promises';

const REPO = 'atul121001/mcpload';
const BASE_URL = `https://github.com/${REPO}/releases/download`;

export interface Platform {
  os: string;
  arch: string;
  ext: string;
  archiveExt: string;
}

export function detectPlatform(): Platform {
  const { platform, arch } = process;
  let os: string;
  let ext = '';
  let archiveExt = '.tar.gz';

  switch (platform) {
    case 'linux': os = 'linux'; break;
    case 'darwin': os = 'darwin'; break;
    case 'win32': os = 'windows'; ext = '.exe'; archiveExt = '.zip'; break;
    default: throw new Error(`Unsupported OS: ${platform}. mcpload supports Linux, macOS, and Windows.`);
  }

  if (arch === 'arm64') return { os, arch: 'arm64', ext, archiveExt };
  if (arch === 'x64') return { os, arch: 'amd64', ext, archiveExt };
  throw new Error(`Unsupported architecture: ${arch}. mcpload requires 64-bit (x64 or arm64).`);
}

export function cacheDir(): string {
  const base = process.env.MCPLOAD_NPM_CACHE_DIR;
  if (base) return base;
  const home = process.env.HOME || process.env.USERPROFILE || tmpdir();
  return join(home, '.mcpload-npm');
}

export function binaryPath(version: string, platform: Platform): string {
  return join(cacheDir(), version, `mcpload${platform.ext}`);
}

async function fetchBuffer(url: string): Promise<Buffer> {
  const res = await fetch(url, { headers: { 'User-Agent': 'mcpload' }, redirect: 'follow' });
  if (!res.ok) throw new Error(`Download failed (${res.status}): ${url}`);
  return Buffer.from(await res.arrayBuffer());
}

export async function latestRelease(): Promise<string> {
  const envVersion = process.env.MCPLOAD_VERSION;
  if (envVersion) return envVersion.startsWith('v') ? envVersion : `v${envVersion}`;
  const res = await fetch(`https://api.github.com/repos/${REPO}/releases/latest`, {
    headers: { 'User-Agent': 'mcpload' },
  });
  if (!res.ok) {
    const locRes = await fetch(`https://github.com/${REPO}/releases/latest`, { redirect: 'manual' });
    if (locRes.status === 302 || locRes.status === 301) {
      const loc = locRes.headers.get('location') || '';
      const match = loc.match(/\/tag\/([^/]+)$/);
      if (match) return match[1];
    }
    throw new Error(`Could not determine latest release (${res.status})`);
  }
  return (await res.json()).tag_name as string;
}


export async function fetchChecksums(version: string): Promise<Map<string, string>> {
  const buf = await fetchBuffer(`${BASE_URL}/${version}/checksums.txt`);
  const map = new Map<string, string>();
  for (const line of buf.toString().split('\n')) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    const [hash, ...rest] = trimmed.split(/\s+/);
    map.set(rest.join(' ').replace(/^\s+/, ''), hash);
  }
  return map;
}

export async function downloadBinary(
  version: string,
  platform: Platform,
  onProgress?: (msg: string) => void,
): Promise<string> {
  const name = `mcpload_${version.replace(/^v/, '')}_${platform.os}_${platform.arch}`;
  const archiveName = `${name}${platform.archiveExt}`;
  const destDir = join(cacheDir(), version);
  mkdirSync(destDir, { recursive: true });

  const targetPath = binaryPath(version, platform);
  if (existsSync(targetPath)) {
    onProgress?.(`Using cached binary: ${targetPath}`);
    return targetPath;
  }

  onProgress?.(`Downloading mcpload ${version} (${platform.os}/${platform.arch})`);
  const tmpDirPath = join(tmpdir(), `mcpload-${Date.now()}-${Math.random().toString(36).slice(2)}`);
  mkdirSync(tmpDirPath, { recursive: true });

  try {
    const archivePath = join(tmpDirPath, archiveName);
    const archiveBuf = await fetchBuffer(`${BASE_URL}/${version}/${archiveName}`);
    writeFileSync(archivePath, archiveBuf);

    onProgress?.('Verifying checksum');
    const checksums = await fetchChecksums(version);
    const expectedHash = checksums.get(archiveName);
    if (!expectedHash) {
      throw new Error(`Checksum not found for ${archiveName} in checksums.txt. Cannot verify integrity.`);
    }
    const hash = createHash('sha256').update(archiveBuf).digest('hex');
    if (hash !== expectedHash) {
      throw new Error(`Checksum mismatch: expected ${expectedHash}, got ${hash}. Download may be corrupt.`);
    }

    onProgress?.('Extracting');
    if (platform.archiveExt === '.zip') {
      const unzip = await import('extract-zip');
      await unzip.default(archivePath, { dir: join(tmpDirPath, 'out') });
      copyFileSync(join(tmpDirPath, 'out', name, 'mcpload.exe'), targetPath);
    } else {
      const tarPath = join(tmpDirPath, `${name}.tar.gz`);
      copyFileSync(archivePath, tarPath);
      await extract({ file: tarPath, cwd: tmpDirPath });
      copyFileSync(join(tmpDirPath, name, 'mcpload'), targetPath);
      chmodSync(targetPath, 0o755);
    }

    onProgress?.(`Installed to ${targetPath}`);
    return targetPath;
  } finally {
    await rm(tmpDirPath, { recursive: true, force: true });
  }
}