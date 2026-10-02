#!/usr/bin/env node
// Generate SYNTHETIC, schema-valid example reports (reproducible: seeded PRNG, fixed timestamps).
// These are not measurements of any real server: the series are modelled on the demo servers, and the
// verdicts are computed from them with the same rules and message strings as `mcpload run`.
//   node report/examples/generate.mjs   ->  report/examples/healthy.json, report/examples/leaky.json
//
// Shape: 40-minute soak, 30 s buckets. Warm-up 0-4 min, constant load 4-36 min, cool-down (zero load) 36-40 min.
// healthy: flat RSS, all verdicts pass.  leaky: RSS rising ~3 MiB/min (R^2 ~0.95), no cool-down recovery.
import { writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const MiB = 1024 * 1024;

const INTERVAL_S = 30;
const WARMUP_END_S = 240;
const LOAD_END_S = 2160;
const COOLDOWN_END_S = 2400;

// ---------- helpers ----------
function mulberry32(seed) {
  return function () {
    seed |= 0; seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
function gauss(rand) {
  // Box-Muller
  const u = 1 - rand(), v = rand();
  return Math.sqrt(-2 * Math.log(u)) * Math.cos(2 * Math.PI * v);
}
const round = (x, d = 0) => { const f = 10 ** d; return Math.round(x * f) / f; };

// ---------- analysis: a port of cmd/mcpload/internal/analysis (DefaultConfig), so the example verdicts
// carry exactly what `mcpload run` would compute for these series ----------
const CFG = {
  leakSlopeMiBPerMin: 1.0, minR2: 0.7, cooldownRecoveryFrac: 0.2, cooldownSkipS: 30, sessionSlopePerMin: 0.5,
  fdSlopePerMin: 1, latencyDriftFrac: 0.5, errorDriftPts: 0.01, baselineWindowS: 120, driftMinR2: 0.5, minPoints: 3,
};
const r2s = (x) => x.toFixed(2);
const sign = (x, d) => (x >= 0 ? '+' : '') + x.toFixed(d); // Go %+.Nf
const fmtG = (x) => String(x); // Go %g for the plain limits used here

function linReg(xs, ys) {
  const n = xs.length;
  const mx = xs.reduce((a, b) => a + b, 0) / n, my = ys.reduce((a, b) => a + b, 0) / n;
  let sxx = 0, sxy = 0, syy = 0;
  for (let i = 0; i < n; i++) { const dx = xs[i] - mx, dy = ys[i] - my; sxx += dx * dx; sxy += dx * dy; syy += dy * dy; }
  const slope = sxy / sxx, icpt = my - slope * mx;
  return { slope, icpt, r2: syy === 0 ? 0 : Math.min(1, Math.max(0, (sxy * sxy) / (sxx * syy))) };
}

function analyze(t, ys, ph, judgeRecovery) {
  const xs = [], vs = [];
  let bSum = 0, bN = 0;
  t.forEach((ti, i) => {
    if (ys[i] == null) return;
    if (ti >= ph.warmupEndS && ti < ph.loadEndS) {
      xs.push(ti / 60); vs.push(ys[i]);
      if (ti < ph.warmupEndS + CFG.baselineWindowS) { bSum += ys[i]; bN++; }
    }
  });
  if (xs.length < Math.max(CFG.minPoints, 2)) throw new Error('not enough points');
  const f = { n: xs.length, ...linReg(xs, vs) };
  f.baseline = bN > 0 ? bSum / bN : vs[0];
  let ss = 0;
  xs.forEach((x, i) => { const d = vs[i] - (f.icpt + f.slope * x); ss += d * d; });
  f.resStd = f.n > 2 ? Math.sqrt(ss / (f.n - 2)) : 0;
  f.loadMinutes = (ph.loadEndS - ph.warmupEndS) / 60;
  f.growth = f.icpt + (f.slope * ph.loadEndS) / 60 - f.baseline;
  f.recovered = null;
  if (judgeRecovery) {
    let cSum = 0, cN = 0;
    t.forEach((ti, i) => {
      if (ys[i] != null && ti >= ph.loadEndS + CFG.cooldownSkipS && ti <= ph.cooldownEndS) { cSum += ys[i]; cN++; }
    });
    if (cN > 0) {
      f.retained = cSum / cN - f.baseline;
      f.recovered = f.retained <= Math.max(CFG.cooldownRecoveryFrac * Math.max(f.growth, 0), 2 * f.resStd);
    }
  }
  return f;
}

function leakVerdict(t, ys, ph, id, signal, scale, limit, what, unit) {
  const f = analyze(t, ys, ph, true);
  const slopeD = f.slope / scale;
  const trending = slopeD > limit && f.r2 >= CFG.minR2;
  const notRecovered = f.recovered === false;
  const num = (x) => (unit === 'MiB' ? `${x.toFixed(2)} ${unit}` : x.toFixed(2));
  const lim = fmtG(limit) + (unit ? ' ' + unit : '');
  const stats = `${num(slopeD)}/min, R²=${r2s(f.r2)}`;
  const notBack = `${num(f.retained / scale)} above the post-warm-up baseline of ${num(f.baseline / scale)}`;
  let message;
  if (trending && notRecovered) message = `${what} grew ${num(slopeD)}/min (R²=${r2s(f.r2)}) under constant load, above the ${lim}/min limit, and did not recover in cool-down (${notBack}).`;
  else if (trending && f.recovered !== null) message = `${what} grew ${num(slopeD)}/min (R²=${r2s(f.r2)}) under constant load, above the ${lim}/min limit; it returned near baseline in cool-down, but the slope alone fails the check.`;
  else if (trending) message = `${what} grew ${num(slopeD)}/min (R²=${r2s(f.r2)}) under constant load, above the ${lim}/min limit; no cool-down samples to judge recovery.`;
  else if (notRecovered) message = `${what} did not recover in cool-down (${notBack}), although it did not grow linearly under load (${stats}).`;
  else if (f.recovered !== null) message = `${what} flat under constant load (${stats}; limit ${lim}/min) and returned near baseline (${num(f.baseline / scale)}) in cool-down.`;
  else message = `${what} flat under constant load (${stats}; limit ${lim}/min); no cool-down samples to judge recovery.`;
  const v = { id, status: trending || notRecovered ? 'fail' : 'pass', signal, slopePerMin: round(f.slope, 4), r2: round(f.r2, 4), baseline: round(f.baseline, 4) };
  if (f.recovered !== null) v.cooldownRecovered = f.recovered;
  v.message = message;
  return v;
}

function latencyDrift(t, ys, ph) {
  const f = analyze(t, ys, ph, false);
  const frac = (f.slope * f.loadMinutes) / f.baseline;
  const warn = frac > CFG.latencyDriftFrac && f.r2 >= CFG.driftMinR2;
  return {
    id: 'latency_drift', status: warn ? 'warn' : 'pass', signal: 'client.p95Ms', slopePerMin: round(f.slope, 4), r2: round(f.r2, 4), baseline: round(f.baseline, 3),
    message: warn
      ? `Client p95 drifted ${sign(frac * 100, 1)}% over the load window (${f.slope.toFixed(1)} ms/min, R²=${r2s(f.r2)}), above the ${(CFG.latencyDriftFrac * 100).toFixed(0)}% limit; consistent with growing GC or queueing pressure.`
      : `Client p95 stable (${sign(frac * 100, 1)}% over the load window, ${f.slope.toFixed(2)} ms/min, R²=${r2s(f.r2)}).`,
  };
}

function errorDrift(t, ys, ph) {
  const f = analyze(t, ys, ph, false);
  const rise = f.slope * f.loadMinutes;
  const warn = rise > CFG.errorDriftPts && f.r2 >= CFG.driftMinR2;
  return {
    id: 'error_drift', status: warn ? 'warn' : 'pass', signal: 'client.errorRate', slopePerMin: round(f.slope, 8), r2: round(f.r2, 4), baseline: round(f.baseline, 6),
    message: warn
      ? `Error rate rose ${(rise * 100).toFixed(2)} percentage points over the load window (R²=${r2s(f.r2)}), above the ${(CFG.errorDriftPts * 100).toFixed(2)}-point limit.`
      : `Error rate did not trend upward over the load window (${sign(rise * 100, 2)} points, baseline ${(f.baseline * 100).toFixed(2)}%).`,
  };
}

// ---------- generator ----------
function build(kind) {
  const leaky = kind === 'leaky';
  const rand = mulberry32(leaky ? 0xbad5eed : 0x600dcafe);
  const t = [];
  for (let s = 0; s <= COOLDOWN_END_S; s += INTERVAL_S) t.push(s);

  const ARRIVAL = 2; // sessions/s at constant load
  const REQS_PER_SESSION = 11; // initialize + tools/list + 9 tools/call (3 rounds x PARALLEL=3)
  const CALL_SHARE = 9 / 11;

  const rps = [], p95 = [], err = [], rss = [], heap = [], fds = [], sess = [];
  for (const s of t) {
    const inWarm = s < WARMUP_END_S, inLoad = s >= WARMUP_END_S && s < LOAD_END_S;
    const loadFrac = inWarm ? s / WARMUP_END_S : inLoad ? 1 : 0;
    const minsIntoLoad = Math.max(0, Math.min(s, LOAD_END_S) - WARMUP_END_S) / 60;

    // client
    const r = loadFrac === 0 ? 0 : ARRIVAL * REQS_PER_SESSION * loadFrac * (1 + 0.03 * gauss(rand));
    rps.push(round(r, 2));
    if (r === 0) { p95.push(null); err.push(null); }
    else {
      const drift = leaky ? 1 + 0.004 * minsIntoLoad : 1; // GC pressure grows with the heap
      p95.push(round((430 + 12 * gauss(rand)) * drift, 1));
      err.push(round(Math.max(0, (leaky ? 0.0089 : 0.0082) + 0.0015 * gauss(rand)), 5));
    }

    // server (prometheus sampler)
    const gcSaw = ((s / INTERVAL_S) % 4) * 1.2 * MiB; // small GC sawtooth
    let base;
    if (!leaky) {
      base = inWarm ? 110 * MiB + 40 * MiB * (s / WARMUP_END_S) : inLoad ? 150 * MiB : 150 * MiB - 6 * MiB * ((s - LOAD_END_S) / (COOLDOWN_END_S - LOAD_END_S));
      rss.push(Math.round(base + gcSaw + 2.2 * MiB * gauss(rand)));
    } else {
      const peak = 180 * MiB + 3 * MiB * ((LOAD_END_S - WARMUP_END_S) / 60);
      base = inWarm ? 120 * MiB + 60 * MiB * (s / WARMUP_END_S) : inLoad ? 180 * MiB + 3 * MiB * minsIntoLoad : peak - 2 * MiB * ((s - LOAD_END_S) / (COOLDOWN_END_S - LOAD_END_S));
      rss.push(Math.round(base + gcSaw + 5.5 * MiB * gauss(rand)));
    }
    heap.push(Math.round(rss[rss.length - 1] * (leaky ? 0.62 : 0.55) + 1.5 * MiB * gauss(rand)));
    fds.push(Math.max(8, Math.round(24 + 30 * loadFrac + 2 * gauss(rand))));
    // ~ sessions in flight: arrival * session lifetime (~4 s) = ~8
    // leaky: ~3 sessions/min are never removed (≈1 MiB retained each -> the ~3 MiB/min RSS slope)
    const stuck = leaky ? Math.round(3 * minsIntoLoad) : 0;
    sess.push(Math.max(0, stuck + Math.round(8 * loadFrac + 1.5 * gauss(rand) * (loadFrac > 0 ? 1 : 0))));
  }

  // ---------- totals ----------
  const reqs = Math.round(rps.reduce((a, b) => a + b * INTERVAL_S, 0));
  const callReqs = Math.round(reqs * CALL_SHARE);
  const mix = { search: 5, fast: 3, slow: 1, big: 1, flaky: 1 };
  const wsum = Object.values(mix).reduce((a, b) => a + b, 0);
  const lat = leaky
    ? { search: [150, 520, 2310, 3890], fast: [14, 41, 88, 240], slow: [309, 352, 440, 980], big: [260, 690, 1210, 2480], flaky: [46, 150, 330, 910] }
    : { search: [118, 372, 716, 1480], fast: [11, 29, 54, 190], slow: [303, 331, 372, 610], big: [205, 518, 902, 1650], flaky: [39, 118, 262, 780] };
  const flakyRate = 0.098; // demo FLAKY_RATE=0.1
  const tools = Object.entries(mix).map(([name, w]) => {
    const n = Math.round((callReqs * w) / wsum);
    let e = name === 'flaky' ? Math.round(n * flakyRate) : name === 'slow' ? (leaky ? 9 : 2) : name === 'search' ? (leaky ? 14 : 3) : 0;
    const [p50, p95_, p99, max] = lat[name];
    return { name, reqs: n, errors: e, errorRate: round(e / n, 5), p50, p95: p95_, p99, max };
  });
  const toolIsError = tools.find((x) => x.name === 'flaky').errors;
  const timeouts = tools.filter((x) => x.name !== 'flaky').reduce((a, x) => a + x.errors, 0);
  const byErrorType = { http: leaky ? 6 : 2, jsonrpc: 0, tool_iserror: toolIsError, timeout: timeouts, session_not_found: 0, header_mismatch: 0, auth: 0 };
  const errors = Object.values(byErrorType).reduce((a, b) => a + b, 0);

  // ---------- thresholds (as built by scenarios/lib/config.js with P95_MS=800 P99_MS=2000, TOOL_BUDGETS default {"flaky":{"errRate":0.2}}) ----------
  const budgets = { search: [800, 2000, 0.01], fast: [800, 2000, 0.01], slow: [800, 2000, 0.01], big: [800, 2000, 0.01], flaky: [800, 2000, 0.2] };
  const thresholds = [];
  for (const tl of tools) {
    const [b95, b99, be] = budgets[tl.name];
    thresholds.push({ metric: `mcp_req_duration{tool:${tl.name}}`, expr: `p(95)<${b95}`, passed: tl.p95 < b95, observed: tl.p95 });
    thresholds.push({ metric: `mcp_req_duration{tool:${tl.name}}`, expr: `p(99)<${b99}`, passed: tl.p99 < b99, observed: tl.p99 });
    thresholds.push({ metric: `mcp_tool_error_rate{tool:${tl.name}}`, expr: `rate<${be}`, passed: tl.errorRate < be, observed: tl.errorRate });
  }
  thresholds.push({ metric: 'mcp_errors{error_type:session_not_found}', expr: 'count<1', passed: true, observed: 0 });
  const failedThresholds = thresholds.filter((x) => !x.passed);

  // ---------- verdicts (port of cmd/mcpload/internal/analysis with DefaultConfig: same math, same message strings) ----------
  const ph = { warmupEndS: WARMUP_END_S, loadEndS: LOAD_END_S, cooldownEndS: COOLDOWN_END_S };
  const verdicts = [
    leakVerdict(t, rss, ph, 'memory_leak', 'server.rssBytes', MiB, CFG.leakSlopeMiBPerMin, 'RSS', 'MiB'),
    leakVerdict(t, sess, ph, 'session_leak', 'server.activeSessions', 1, CFG.sessionSlopePerMin, 'Active sessions', ''),
    leakVerdict(t, fds, ph, 'fd_leak', 'server.openFds', 1, CFG.fdSlopePerMin, 'Open file descriptors', ''),
    latencyDrift(t, p95, ph),
    errorDrift(t, err, ph),
    { id: 'session_not_found', status: 'pass', signal: 'mcp_errors{error_type:session_not_found}', message: 'No 404 session-not-found responses.' },
    {
      id: 'threshold', status: failedThresholds.length ? 'fail' : 'pass', signal: 'thresholds',
      message: failedThresholds.length
        ? `${failedThresholds.length} of ${thresholds.length} thresholds failed: ${failedThresholds.map((x) => `${x.metric} ${x.expr} (observed ${x.observed})`).join('; ')}.`
        : `All ${thresholds.length} thresholds passed.`,
    },
  ];

  const startedAt = leaky ? '2026-09-29T14:00:00Z' : '2026-09-29T13:00:00Z';
  const endedAt = new Date(Date.parse(startedAt) + COOLDOWN_END_S * 1000).toISOString().replace('.000Z', 'Z');
  return {
    schemaVersion: '1',
    tool: { name: 'mcpload', version: '0.1.0-dev' },
    run: {
      id: leaky ? 'example_synthetic_ts-leaky' : 'example_synthetic_ts-healthy',
      startedAt, endedAt, durationS: COOLDOWN_END_S,
      scenario: 'soak',
      protocol: '2025-06-18',
      target: leaky ? { url: 'http://localhost:3002/mcp', label: 'ts-leaky (synthetic example)' } : { url: 'http://localhost:3001/mcp', label: 'ts-healthy (synthetic example)' },
      git: { sha: leaky ? '9f3c2e1a7b4d5c6e8f901a2b3c4d5e6f7a8b9c0d' : '4a1b2c3d4e5f60718293a4b5c6d7e8f901234567', ref: 'refs/heads/main' },
      k6Version: 'v2.3.0',
      load: { executor: 'constant-arrival-rate', vus: 20, maxVus: 100, arrivalRate: ARRIVAL, arrivalTimeUnitS: 1 },
    },
    phases: { warmupEndS: WARMUP_END_S, loadEndS: LOAD_END_S, cooldownEndS: COOLDOWN_END_S },
    summary: { reqs, errors, errorRate: round(errors / reqs, 6), byErrorType },
    tools,
    thresholds,
    series: {
      intervalS: INTERVAL_S,
      t,
      client: { p95Ms: p95, errorRate: err, rps },
      server: { sampler: 'prometheus', rssBytes: rss, heapBytes: heap, openFds: fds, activeSessions: sess },
    },
    verdicts,
    payloadsIncluded: false,
  };
}

for (const kind of ['healthy', 'leaky']) {
  const r = build(kind);
  writeFileSync(join(here, `${kind}.json`), JSON.stringify(r, null, 2) + '\n');
  const m = r.verdicts.find((v) => v.id === 'memory_leak');
  console.log(`${kind}.json: ${r.series.t.length} samples, memory_leak=${m.status} slope=${(m.slopePerMin / MiB).toFixed(2)} MiB/min r2=${m.r2} recovered=${m.cooldownRecovered}`);
}
