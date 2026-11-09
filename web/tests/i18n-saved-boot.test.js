/**
 * A saved language beats whatever the browser asks for.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`).
 */

import test from 'node:test';
import assert from 'node:assert/strict';

Object.defineProperty(globalThis, 'navigator', {
  value: { languages: ['ru-RU'], language: 'ru-RU' },
  configurable: true,
});
globalThis.localStorage = { getItem: (k) => (k === 'of_locale' ? 'de' : null), setItem() {}, removeItem() {} };
globalThis.document = { documentElement: {} };

const { getLocale } = await import('../admin/app/core/i18n.js');

test('the saved choice wins over the browser', () => {
  assert.equal(getLocale(), 'de');
});
