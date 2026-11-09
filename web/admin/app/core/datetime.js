/**
 * Shared date/time formatting. Every formatter uses the interface language
 * chosen in settings. Contract: accepts ISO string, Date, or epoch number; falsy
 * input yields '' and an unparseable value is returned unchanged.
 */

import { intlTag, t, tn } from './i18n.js';

/**
 * Coerce a value to a valid Date, or null when it can't be parsed.
 * @param {string|number|Date} value
 * @returns {Date|null}
 */
function toDate(value) {
  const d = value instanceof Date ? value : new Date(value);
  return isNaN(d.getTime()) ? null : d;
}

/** Time only, hour + minute, locale-aware ("2:34 PM" or "14:34"). */
export function formatTime(value) {
  if (!value) return '';
  const d = toDate(value);
  if (!d) return value;
  return d.toLocaleTimeString(intlTag(), { hour: 'numeric', minute: '2-digit' });
}

/** Time with seconds, locale-aware ("2:34:07 PM" or "14:34:07"). */
export function formatTimeWithSeconds(value) {
  if (!value) return '';
  const d = toDate(value);
  if (!d) return value;
  return d.toLocaleTimeString(intlTag(), { hour: 'numeric', minute: '2-digit', second: '2-digit' });
}

/** Date only, locale-aware short date ("6/18/2026" or "18-06-2026"). */
export function formatDate(value) {
  if (!value) return '';
  const d = toDate(value);
  if (!d) return value;
  return d.toLocaleDateString(intlTag());
}

/** Full date + time, locale-aware (equivalent to Date.toLocaleString). */
export function formatDateTime(value) {
  if (!value) return '';
  const d = toDate(value);
  if (!d) return value;
  return d.toLocaleString(intlTag());
}

/**
 * Relative time ("just now" / "5m ago" / "3h ago" / "2d ago"), worded from the
 * catalog; beyond a week falls back to a locale-aware short date.
 */
export function formatRelative(value) {
  if (!value) return '';
  const d = toDate(value);
  if (!d) return value;
  const sec = Math.floor((Date.now() - d.getTime()) / 1000);
  if (sec < 60) return t('time.justNow');
  const min = Math.floor(sec / 60);
  if (min < 60) return tn('time.minutesAgo', min);
  const hr = Math.floor(min / 60);
  if (hr < 24) return tn('time.hoursAgo', hr);
  const day = Math.floor(hr / 24);
  if (day < 7) return tn('time.daysAgo', day);
  return formatDate(d);
}
