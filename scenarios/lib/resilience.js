// Pure helpers for long-lived sessions and reconnect storms (scenarios/long-lived.js, scenarios/reconnect-storm.js).
// No k6 imports, so they can be unit tested with plain Node (resilience.test.mjs).

/** params._meta key that carries the client's call id (servers can record it to count executions per call). */
export const CALL_ID_KEY = 'io.mcpload/callId';

// Error types that mean the transport or the session is gone, not that one call failed.
const TRANSPORT = ['http', 'timeout'];
// Error types that mean the session itself is gone: the server dropped it, or (stdio) its process exited.
const GONE = ['session_not_found', 'process_exit'];

/**
 * Why a session must be treated as broken after a batch of call errors (each `{type}` or null for a success),
 * or '' when it is still usable:
 *   - any session_not_found: the server no longer has the session (restart, idle reaping, another replica);
 *   - any process_exit: the stdio server process of the session exited;
 *   - every call failed with a transport error (http without a JSON-RPC answer, timeout): the server is
 *     unreachable. One failed call among successes is not enough.
 * Tool errors (tool_iserror) and JSON-RPC errors never break a session.
 */
export function breakCause(errors) {
  if (!errors || errors.length === 0) return '';
  for (const e of errors) if (e && GONE.indexOf(e.type) >= 0) return e.type;
  if (errors.every((e) => e && TRANSPORT.indexOf(e.type) >= 0)) return errors[0].type;
  return '';
}

/** Whether a call that failed with `error` should be retried on a new session (RETRY_ON_ERROR). */
export function retryable(error) {
  return !!error && (GONE.indexOf(error.type) >= 0 || TRANSPORT.indexOf(error.type) >= 0);
}

/**
 * Delay in ms before reconnect attempt `attempt` (1 = the first attempt after a break): 0 for the first one
 * (agents reconnect at once, which is the storm), then baseMs * 2^(attempt-2) capped at maxMs. `jitter` (0..1)
 * takes up to that fraction off at random ("equal jitter" at 0.5, "full jitter" at 1). rnd is Math.random.
 */
export function backoffMs(attempt, baseMs, maxMs, jitter, rnd) {
  if (attempt <= 1 || !(baseMs > 0)) return 0;
  const d = Math.min(maxMs > 0 ? maxMs : Infinity, baseMs * Math.pow(2, attempt - 2));
  const j = Math.min(1, Math.max(0, jitter || 0));
  return Math.round(d * (1 - j * (rnd || Math.random)()));
}

/** Call ids `${prefix}-${vu}-${n}` (n counts from 1 per VU): unique across a run when prefix is unique per run. */
export function callIdFactory(prefix, vu) {
  let n = 0;
  return () => `${prefix}-${vu}-${++n}`;
}

/**
 * Which part of a session a call falls in, for comparing late with early calls of the same sessions: 'early'
 * in the first third of the planned lifetime, 'late' in the last third, '' in between.
 */
export function sessionAge(elapsedS, plannedS) {
  if (!(plannedS > 0)) return '';
  if (elapsedS < plannedS / 3) return 'early';
  if (elapsedS >= (2 * plannedS) / 3) return 'late';
  return '';
}

/** Seconds VU `vu` (1-based) of `vus` waits before opening its session: spread evenly over the warm-up. */
export function staggerS(vu, vus, warmupS) {
  if (!(warmupS > 0) || !(vus > 0)) return 0;
  return ((Math.max(1, vu) - 1) % vus) * (warmupS / vus);
}
