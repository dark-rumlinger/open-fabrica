import * as toast from './toast.js';
import { humanizeError } from './errors.js';

// Single-flight guard: fn references currently in flight. Call sites that pass
// a stable function reference (a named handler shared by a button and a
// keyboard shortcut, or crud-panel's `run`) get concurrent-invocation dedup;
// sites that build a fresh arrow per click are already guarded by the modal's
// action-button pending/disabled wrapper.
const inFlight = new WeakSet();

/**
 * Wraps a mutation in the shared try/toast/catch shape. Resolves `false` on
 * failure, on a handled validation failure (`fn` resolving false), or when the
 * same `fn` reference is already in flight; the modal treats `false` as
 * "don't close". Success resolves `undefined` (modal closes), shows the
 * optional `success` toast, and runs `onDone`. `errorPrefix` prepends context
 * to the failure toast.
 */
export async function runAction(fn, { success, onDone, errorPrefix } = {}) {
  if (inFlight.has(fn)) return false;
  inFlight.add(fn);
  try {
    const result = await fn();
    if (result === false) return false;
    if (success) toast.success(success);
    if (onDone) onDone();
  } catch (err) {
    toast.error((errorPrefix || '') + humanizeError(err));
    return false;
  } finally {
    inFlight.delete(fn);
  }
}
