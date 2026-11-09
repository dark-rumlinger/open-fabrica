/**
 * What i18n.js does at load time: a saved choice beats the browser, the browser
 * beats the default. Separate file because the language is read once, on import.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`).
 */

import test from 'node:test';
import assert from 'node:assert/strict';

Object.defineProperty(globalThis, 'navigator', {
  value: { languages: ['ru-RU', 'en-US'], language: 'ru-RU' },
  configurable: true,
});
globalThis.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
globalThis.document = { documentElement: {} };

const { getLocale, getDir, intlTag, t } = await import('../admin/app/core/i18n.js');

test('with nothing saved the browser language is used', () => {
  assert.equal(getLocale(), 'ru');
  assert.equal(intlTag(), 'ru');
  assert.equal(getDir(), 'ltr');
  assert.equal(globalThis.document.documentElement.lang, 'ru');
  assert.equal(t('common.cancel'), 'Отмена');
});
