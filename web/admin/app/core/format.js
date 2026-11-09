/**
 * Number, size, money and duration formatting in the interface language.
 *
 * Every figure the UI shows goes through here, so a decimal comma, a grouping
 * space, a currency symbol's position and a unit's spelling follow the language
 * instead of being English with the digits swapped in.
 */

import { intlTag } from './i18n.js';

const cache = new Map();

/** A NumberFormat for the interface language, built once per option set. */
function formatter(options) {
  const key = intlTag() + JSON.stringify(options || {});
  let f = cache.get(key);
  if (!f) {
    f = new Intl.NumberFormat(intlTag(), options);
    cache.set(key, f);
  }
  return f;
}

/** 12345.6 -> "12,345.6" (en), "12.345,6" (de), "12 345,6" (fr). */
export function formatNumber(n, options) {
  return formatter(options).format(Number(n) || 0);
}

/** 128000 -> "128K" (en). Order of magnitude only, one decimal at most. */
export function formatCompact(n) {
  return formatter({ notation: 'compact', maximumFractionDigits: 1 }).format(Number(n) || 0);
}

/** A size in bytes, as bytes, KB or MB. */
export function formatBytes(bytes) {
  const n = Number(bytes) || 0;
  if (n < 1024) return formatter({ style: 'unit', unit: 'byte', unitDisplay: 'short' }).format(n);
  if (n < 1024 * 1024) {
    return formatter({ style: 'unit', unit: 'kilobyte', unitDisplay: 'short', maximumFractionDigits: 1 }).format(n / 1024);
  }
  return formatter({ style: 'unit', unit: 'megabyte', unitDisplay: 'short', maximumFractionDigits: 1 }).format(n / (1024 * 1024));
}

/** US dollars, with cents unless `fractionDigits` says otherwise. */
export function formatUsd(n, fractionDigits = 2) {
  return formatter({
    style: 'currency',
    currency: 'USD',
    minimumFractionDigits: fractionDigits,
    maximumFractionDigits: fractionDigits,
  }).format(Number(n) || 0);
}

/**
 * A price per million tokens: whole dollars stay whole ($3), cents keep their
 * cents ($0.15), nothing is padded.
 */
export function formatPrice(v) {
  return formatter({
    style: 'currency',
    currency: 'USD',
    minimumFractionDigits: 0,
    maximumFractionDigits: 2,
  }).format(Number(v) || 0);
}

/**
 * A span in milliseconds: ms under a second, seconds (one decimal) under a
 * minute, and `Xm Ys` above. Units follow the interface language.
 */
export function formatDuration(ms) {
  const n = Number(ms) || 0;
  if (n < 1000) return formatter({ style: 'unit', unit: 'millisecond', unitDisplay: 'narrow' }).format(n);
  if (n < 60000) return formatter({ style: 'unit', unit: 'second', unitDisplay: 'narrow', maximumFractionDigits: 1 }).format(n / 1000);
  const totalSec = Math.round(n / 1000);
  const min = formatter({ style: 'unit', unit: 'minute', unitDisplay: 'narrow' }).format(Math.floor(totalSec / 60));
  const sec = formatter({ style: 'unit', unit: 'second', unitDisplay: 'narrow' }).format(totalSec % 60);
  return `${min} ${sec}`;
}

/** A ratio such as an overhead multiple: 1.5 -> "1.5x". */
export function formatRatio(n) {
  return formatter({ maximumFractionDigits: 1, minimumFractionDigits: 1 }).format(Number(n) || 0) + 'x';
}
