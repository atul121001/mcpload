#!/usr/bin/env node
// Generate the DATA of the SYNTHETIC, schema-valid example reports (reproducible: seeded PRNG, fixed timestamps).
// These are not measurements of any real server: the series are modelled on the demo servers.
//
// This script writes everything except the verdicts (it leaves `verdicts: []`). The verdicts are computed by
// the real analysis code of the CLI, so the examples can never drift from what `mcpload run` reports:
//
//   node report/examples/generate.mjs                                   # data -> healthy.json, leaky.json
//   cd cmd/mcpload && go test ./internal/analysis -run TestExamples -update   # verdicts (Go)
//   node report/render.mjs report/examples/healthy.json report/examples/healthy.html
//   node report/render.mjs report/examples/leaky.json report/examples/leaky.html
//
// Shape: 40-minute soak, 30 s buckets. Warm-up 0-4 min, constant load 4-36 min, cool-down (zero load) 36-40 min.
// healthy: flat RSS/heap, sessions and fds return to idle, stable per-tool latency, generator well within capacity.
// leaky: RSS/heap rising ~3 MiB/min with no cool-down recovery, ~3 sessions/min never closed, the `search`
//        tool's p95 drifting up, a few dropped iterations late in the run.
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

// ---------- generator ----------
function build(kind) {
  const leaky = kind === 'leaky';
  const rand = mulberry32(leaky ? 0xbad5eed : 0x600dcafe);
  const t = [];
  for (let s = 0; s <= COOLDOWN_END_S; s += INTERVAL_S) t.push(s);

  const ARRIVAL = 2; // sessions/s at constant load
  const REQS_PER_SESSION = 11; // initialize + tools/list + 9 tools/call (3 rounds x PARALLEL=3)
  const CALL_SHARE = 9 / 11;

  // Per-tool latency (successful calls only), ms: [p50, p95, p99, max].
  const lat = leaky
    ? { search: [150, 520, 2310, 3890], fast: [14, 41, 88, 240], slow: [309, 352, 440, 980], big: [260, 690, 1210, 2480], flaky: [46, 150, 330, 910] }
    : { search: [118, 372, 716, 1480], fast: [11, 29, 54, 190], slow: [303, 331, 372, 610], big: [205, 518, 902, 1650], flaky: [39, 118, 262, 780] };
  const toolNames = Object.keys(lat);

  const rps = [], p95 = [], err = [], dropped = [], rss = [], heap = [], fds = [], sess = [];
  const toolP95 = Object.fromEntries(toolNames.map((n) => [n, []]));
  for (const s of t) {
    const inWarm = s < WARMUP_END_S, inLoad = s >= WARMUP_END_S && s < LOAD_END_S;
    const loadFrac = inWarm ? s / WARMUP_END_S : inLoad ? 1 : 0;
    const minsIntoLoad = Math.max(0, Math.min(s, LOAD_END_S) - WARMUP_END_S) / 60;

    // client
    const r = loadFrac === 0 ? 0 : ARRIVAL * REQS_PER_SESSION * loadFrac * (1 + 0.03 * gauss(rand));
    rps.push(round(r, 2));
    if (r === 0) {
      p95.push(null); err.push(null);
      for (const n of toolNames) toolP95[n].push(null);
    } else {
      const drift = leaky ? 1 + 0.004 * minsIntoLoad : 1; // GC pressure grows with the heap
      p95.push(round((430 + 12 * gauss(rand)) * drift, 1));
      err.push(round(Math.max(0, (leaky ? 0.0089 : 0.0082) + 0.0015 * gauss(rand)), 5));
      for (const n of toolNames) {
        // leaky: search scans the ever-growing session map, so its p95 climbs ~3%/min
        const d = leaky && n === 'search' ? 0.62 + 0.03 * minsIntoLoad : 1;
        toolP95[n].push(round(Math.max(1, lat[n][1] * d * (1 + 0.04 * gauss(rand))), 1));
      }
    }
    // dropped iterations per bucket (arrival-rate executor could not start them on time)
    dropped.push(leaky && inLoad && minsIntoLoad > 28 ? Math.round(2 + 2 * rand()) : 0);

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
  const flakyRate = 0.098; // demo FLAKY_RATE=0.1
  const tools = Object.entries(mix).map(([name, w]) => {
    const n = Math.round((callReqs * w) / wsum);
    const e = name === 'flaky' ? Math.round(n * flakyRate) : name === 'slow' ? (leaky ? 9 : 2) : name === 'search' ? (leaky ? 14 : 3) : 0;
    const [p50, p95_, p99, max] = lat[name];
    return { name, reqs: n, errors: e, errorRate: round(e / n, 5), p50, p95: p95_, p99, max };
  });
  const toolIsError = tools.find((x) => x.name === 'flaky').errors;
  const timeouts = tools.filter((x) => x.name !== 'flaky').reduce((a, x) => a + x.errors, 0);
  const byErrorType = { http: leaky ? 6 : 2, jsonrpc: 0, tool_iserror: toolIsError, timeout: timeouts, session_not_found: 0, header_mismatch: 0, auth: 0 };
  const errors = Object.values(byErrorType).reduce((a, b) => a + b, 0);
  const droppedIterations = dropped.reduce((a, b) => a + b, 0);
  const iterations = Math.round(reqs / REQS_PER_SESSION);

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
  // a throughput floor: mcp_reqs is a Counter, so its "rate" is requests per second (not a percentage)
  const avgRps = round(reqs / COOLDOWN_END_S, 2);
  thresholds.push({ metric: 'mcp_reqs', expr: 'rate>5', passed: avgRps > 5, observed: avgRps });

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
      generator: leaky ? { cores: 8, cpuAvgPct: 22.4, cpuMaxPct: 38.9 } : { cores: 8, cpuAvgPct: 18.1, cpuMaxPct: 29.6 },
    },
    phases: { warmupEndS: WARMUP_END_S, loadEndS: LOAD_END_S, cooldownEndS: COOLDOWN_END_S },
    summary: { reqs, errors, errorRate: round(errors / reqs, 6), byErrorType, iterations, droppedIterations, toolErrors: toolIsError },
    tools,
    thresholds,
    series: {
      intervalS: INTERVAL_S,
      t,
      client: { p95Ms: p95, errorRate: err, rps, droppedIterations: dropped },
      server: { sampler: 'prometheus', rssBytes: rss, heapBytes: heap, openFds: fds, activeSessions: sess },
      tools: Object.fromEntries(toolNames.map((n) => [n, { p95Ms: toolP95[n] }])),
    },
    verdicts: [], // filled by `go test ./internal/analysis -run TestExamples -update`
    payloadsIncluded: false,
  };
}

for (const kind of ['healthy', 'leaky']) {
  const r = build(kind);
  writeFileSync(join(here, `${kind}.json`), JSON.stringify(r, null, 2) + '\n');
  console.log(`${kind}.json: ${r.series.t.length} samples (data only; now run the Go -update step to fill verdicts)`);
}
