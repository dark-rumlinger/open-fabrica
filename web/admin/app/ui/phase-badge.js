/**
 * The phase badge, built from the shared phase view (phase-state.js). Every
 * surface that shows a project's phase renders it through here.
 */

import { h, clear } from '../core/dom.js';
import { describePhase } from './phase-state.js';

/**
 * Builds the badge. With `withReason` the one-line reason (for example "2
 * questions need your answer") sits under it when there is one; without it the
 * reason is still available on hover. A pause after an error is the exception:
 * the badge stands alone in its warning colour, with the error line kept to the
 * hover tooltip rather than wrapping onto a second line.
 */
export function createPhaseBadge(phase, { withReason = false } = {}) {
  const view = describePhase(phase);
  const badge = h('span', {
    className: `badge badge--${view.kind}`,
    title: view.sentence,
  }, view.label);
  if (!withReason || !view.reason || view.error) return badge;
  return h('span', { className: 'phase-badge' }, [
    badge,
    h('span', { className: 'form-hint phase-badge__reason' }, view.reason),
  ]);
}

/** The small status dot for a phase, used where a badge is too wide. */
export function createPhaseDot(phase) {
  const view = describePhase(phase);
  return h('span', {
    className: `status-dot${view.dot ? ' ' + view.dot : ''}`,
    title: view.label,
  });
}

/** Replaces the contents of a host element with a fresh badge. */
export function setPhaseBadge(host, phase, opts) {
  clear(host);
  host.appendChild(createPhaseBadge(phase, opts));
}
