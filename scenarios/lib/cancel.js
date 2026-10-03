// Pure helpers (no k6 imports) for cancellation testing: which calls a session cancels, and when.
// Unit-tested by cancel.test.mjs.

/**
 * Parse the cancellation knobs.
 *   CANCEL_RATE      share of eligible tools/call to cancel, 0..1 (default 0: never)
 *   CANCEL_AFTER_MS  cancel a call that has no response this many ms after it was sent (default 150)
 *   CANCEL_TOOLS     comma-separated tool names eligible for cancelling (default: every tool)
 * `get(name)` returns the raw value or undefined. Returns { rate, afterMs, tools: string[] | null }.
 */
export function parseCancelConfig(get) {
  const num = (name, def) => {
    const v = get(name);
    if (v === undefined || v === '') return def;
    const n = Number(v);
    if (!Number.isFinite(n)) throw new Error(`${name} must be a number, got '${v}'`);
    return n;
  };
  const rate = num('CANCEL_RATE', 0);
  if (rate < 0 || rate > 1) throw new Error(`CANCEL_RATE must be between 0 and 1, got '${get('CANCEL_RATE')}'`);
  const afterMs = num('CANCEL_AFTER_MS', 150);
  if (!(afterMs > 0)) throw new Error(`CANCEL_AFTER_MS must be > 0, got '${get('CANCEL_AFTER_MS')}'`);
  const raw = get('CANCEL_TOOLS');
  const tools =
    raw === undefined || String(raw).trim() === ''
      ? null
      : String(raw)
          .split(',')
          .map((s) => s.trim())
          .filter((s) => s !== '');
  return { rate, afterMs, tools };
}

/**
 * The per-call options for one call to `name`: { cancelAfterMs } when this call is to be cancelled (it is
 * eligible and `rnd` < rate), else undefined. `rnd` in [0,1) (default Math.random()).
 */
export function cancelOptions(name, cc, rnd) {
  if (!cc || !(cc.rate > 0)) return undefined;
  if (cc.tools && cc.tools.indexOf(name) < 0) return undefined;
  const r = rnd === undefined ? Math.random() : rnd;
  return r < cc.rate ? { cancelAfterMs: cc.afterMs } : undefined;
}
