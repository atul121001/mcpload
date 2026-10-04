import { readFileSync, writeFileSync, existsSync } from 'node:fs';
import { join } from 'node:path';
import { cacheDir } from './downloader.js';

const CACHE_FILE = 'cache.json';

export interface CacheEntry {
  version: string;
  checkedAt: string;
}

export function cachePath(): string {
  return join(cacheDir(), CACHE_FILE);
}

export function readCache(): CacheEntry | null {
  const path = cachePath();
  if (!existsSync(path)) return null;
  try {
    return JSON.parse(readFileSync(path, 'utf-8'));
  } catch {
    return null;
  }
}

export function writeCache(entry: CacheEntry): void {
  const path = cachePath();
  writeFileSync(path, JSON.stringify(entry, null, 2));
}

export function cachedVersion(): string | null {
  return readCache()?.version ?? null;
}

export function needsUpdateCheck(): boolean {
  const entry = readCache();
  if (!entry) return true;
  const lastCheck = new Date(entry.checkedAt).getTime();
  const now = Date.now();
  return (now - lastCheck) > 24 * 60 * 60 * 1000; // 24 hours
}