/**
 * Shared utility functions, keeps views DRY.
 */

/**
 * Safely parse a JSON string, returning a fallback on failure.
 * @param {string} raw - The JSON string to parse.
 * @param {*} [fallback=null] - Value returned if parsing fails or raw is falsy.
 * @returns {*} Parsed value or fallback.
 */
export function safeJsonParse(raw, fallback = null) {
  if (!raw || typeof raw !== 'string') return fallback;
  try { return JSON.parse(raw); } catch { return fallback; }
}

/**
 * The one canonical HTML escaper for the admin app; import it rather than
 * re-declaring a private copy. Prefer building DOM with h()/textContent;
 * use this only when you must produce raw HTML.
 */
export function escapeHtml(str) {
  return String(str ?? '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

/**
 * Check whether an SSE event payload belongs to the given project.
 * Replaces the repeated `!data || String(data.project_id) !== String(projectId)` guard.
 * @param {Object} data - Event payload.
 * @param {string|number} projectId - The current project ID.
 * @returns {boolean} True if the event is for this project.
 */
export function isForProject(data, projectId) {
  return !!data && String(data.project_id) === String(projectId);
}

/**
 * Map an async fn over items with at most `limit` promises in flight at once,
 * returning results in the original order. fn receives (item, index). Use it for
 * per-row fan-out (one request per project, say) so a long list loads in parallel
 * without opening every request at once.
 * @template T, R
 * @param {T[]} items
 * @param {number} limit - Max concurrent calls.
 * @param {(item: T, index: number) => Promise<R>} fn
 * @returns {Promise<R[]>}
 */
export async function mapWithConcurrency(items, limit, fn) {
  const results = new Array(items.length);
  let next = 0;
  const workers = Array.from({ length: Math.min(limit, items.length) || 0 }, async () => {
    while (next < items.length) {
      const idx = next++;
      results[idx] = await fn(items[idx], idx);
    }
  });
  await Promise.all(workers);
  return results;
}
