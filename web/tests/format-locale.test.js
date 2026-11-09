/**
 * core/format.js follows the saved interface language. A separate file because the
 * language is read once, when i18n.js loads.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`).
 */

import test from 'node:test';
import assert from 'node:assert/strict';

globalThis.localStorage = { getItem: (k) => (k === 'of_locale' ? 'de' : null), setItem() {}, removeItem() {} };
globalThis.document = { documentElement: {} };

const { formatNumber, formatUsd, formatPrice, formatDuration, formatRatio } =
  await import('../admin/app/core/format.js');

test('German uses a decimal comma, a grouping dot and a trailing currency sign', () => {
  assert.equal(formatNumber(1234567), '1.234.567');
  assert.match(formatUsd(3.5), /^3,50\s\$$/);
  assert.match(formatPrice(0.15), /^0,15\s\$$/);
  assert.match(formatDuration(1500), /^1,5\s?\S+$/);
  // Minutes tier: minute part then second part, both localized.
  assert.match(formatDuration(90000), /1.*\s.*30/);
  assert.equal(formatRatio(1.25), '1,3x');
});
