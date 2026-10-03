// Pure helpers for scenarios/step-load.js (no k6 imports, so Node can unit-test them).

/**
 * Concurrency levels for a step-load run, strictly increasing positive integers.
 *   steps   "10,25,50" (wins when set)
 *   start, factor, max   geometric levels start, start*factor, ... up to max (used when steps is unset)
 */
export function stepLevels({ steps, start, factor, max }) {
  let levels;
  if (steps !== undefined && steps !== null && String(steps).trim() !== '') {
    levels = String(steps)
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean)
      .map((s) => {
        const n = Number(s);
        if (!Number.isInteger(n) || n < 1) throw new Error(`STEPS must be a comma-separated list of positive integers, got '${s}'`);
        return n;
      });
  } else {
    if (!(start >= 1)) throw new Error(`START must be >= 1, got '${start}'`);
    if (!(factor > 1)) throw new Error(`STEP_FACTOR must be > 1, got '${factor}'`);
    if (!(max >= start)) throw new Error(`MAX_VUS must be >= START, got '${max}'`);
    levels = [];
    for (let v = start; Math.round(v) <= max; v *= factor) levels.push(Math.round(v));
  }
  for (let i = 1; i < levels.length; i++) {
    if (levels[i] <= levels[i - 1]) throw new Error(`step levels must increase: ${levels.join(',')}`);
  }
  if (!levels.length) throw new Error('no step levels');
  return levels;
}

/**
 * ramping-vus stages and measurement windows for levels held holdS seconds each, with a rampS-second ramp
 * before every step (0 -> first level, then level -> next level). Only the hold of each step is measured:
 * windows[i] = { vus, from, to } in seconds since the scenario started.
 */
export function stepSchedule(levels, holdS, rampS) {
  const stages = [];
  const windows = [];
  let t = 0;
  for (const vus of levels) {
    stages.push({ duration: `${rampS}s`, target: vus });
    t += rampS;
    stages.push({ duration: `${holdS}s`, target: vus });
    windows.push({ vus, from: t, to: t + holdS });
    t += holdS;
  }
  return { stages, windows, totalS: t };
}

/** The window holding elapsed second `s` (from <= s < to), or null during a ramp or after the last step. */
export function stepAt(windows, s) {
  for (const w of windows) if (s >= w.from && s < w.to) return w;
  return null;
}
