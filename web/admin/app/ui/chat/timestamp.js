/**
 * Compact chat timestamps, shared by the message renderers (message.js) and the
 * chat cards (cards/*.js). It lives in its own module so a card can render a time
 * without importing message.js - which would be an import cycle, since message.js
 * re-exports the card renderers.
 */

import { h } from '../../core/dom.js';
import { t, intlTag } from '../../core/i18n.js';
import { formatTime } from '../../core/datetime.js';

/**
 * Day prefix relative to now: "" for today, "Yesterday" for yesterday, a short
 * weekday within the last week, and a short date beyond that. Lets long
 * histories read like a journal instead of a wall of bare clock times.
 */
function dayPrefix(d) {
  const startOf = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate());
  const days = Math.round((startOf(new Date()) - startOf(d)) / 86400000);
  if (days <= 0) return '';
  if (days === 1) return t('chat.yesterday');
  if (days < 7) return d.toLocaleDateString(intlTag(), { weekday: 'short' });
  return d.toLocaleDateString(intlTag(), { month: 'short', day: 'numeric' });
}

/**
 * Format a compact timestamp (e.g., "2:34 PM", "Yesterday · 2:34 PM").
 * @param {string} [createdAt], ISO timestamp from the server; defaults to now.
 */
export function timestamp(createdAt) {
  const d = createdAt ? new Date(createdAt) : new Date();
  const time = formatTime(d);
  const prefix = dayPrefix(d);
  return h('span', { className: 'message__time' }, prefix ? `${prefix} · ${time}` : time);
}
