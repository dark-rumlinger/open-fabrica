/**
 * Single source of truth for per-project pending-question counts (top bar,
 * bottom-nav, project badges, tab indicator). The map stays under the
 * 'pendingQuestions' state key so existing watchers keep firing; every
 * mutation must go through here.
 */

import * as state from './state.js';

const KEY = 'pendingQuestions';

function snapshot() {
  return { ...(state.get(KEY) || {}) };
}

/** Pending-question count for one project (0 when unknown). */
export function pendingFor(projectId) {
  return (state.get(KEY) || {})[projectId] || 0;
}

/** Total pending questions across every project. */
export function totalPending() {
  return Object.values(state.get(KEY) || {}).reduce((sum, n) => sum + (n || 0), 0);
}

/** Adjust one project's count by delta (negative to decrement), clamped at 0. */
export function bumpPending(projectId, delta) {
  if (projectId == null) return;
  const map = snapshot();
  map[projectId] = Math.max(0, (map[projectId] || 0) + delta);
  state.set(KEY, map);
}

/** Set one project's count to an exact value (e.g. from a fresh server fetch). */
export function setPending(projectId, count) {
  if (projectId == null) return;
  const map = snapshot();
  map[projectId] = Math.max(0, count || 0);
  state.set(KEY, map);
}

/** Rebuild the whole map from a list of pending question rows. */
export function setPendingFromList(questions) {
  const map = {};
  for (const q of questions || []) map[q.project_id] = (map[q.project_id] || 0) + 1;
  state.set(KEY, map);
}

/** Subscribe to count changes; auto-unsubscribes with the given scope if passed. */
export function watchPending(cb, scope) {
  return state.select(KEY, cb, scope);
}
