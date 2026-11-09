/**
 * First-visit language: with nothing saved, the interface follows the browser's
 * preferred languages and falls back to English.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`).
 */

import test from 'node:test';
import assert from 'node:assert/strict';

const { matchLocale, SUPPORTED } = await import('../admin/app/core/i18n.js');

test('the first preference we serve wins', () => {
  assert.equal(matchLocale(['ja-JP', 'en-US']), 'ja');
  assert.equal(matchLocale(['es-MX', 'es', 'en']), 'es');
  assert.equal(matchLocale(['de']), 'de');
});

test('unserved languages are skipped in preference order', () => {
  assert.equal(matchLocale(['it-IT', 'de-DE', 'en']), 'de');
  assert.equal(matchLocale(['it', 'nl']), 'en');
  assert.equal(matchLocale(['sv-SE', 'fr-CA']), 'fr');
});

test('English is the answer when nothing matches or nothing is given', () => {
  assert.equal(matchLocale([]), 'en');
  assert.equal(matchLocale(undefined), 'en');
  assert.equal(matchLocale(['', null, 42, 'xx']), 'en');
});

test('only the language counts, and case or underscores do not matter', () => {
  assert.equal(matchLocale(['pt-PT']), 'pt');
  assert.equal(matchLocale(['PT_br']), 'pt');
  assert.equal(matchLocale([' ar-EG ']), 'ar');
  assert.equal(matchLocale(['hi-IN']), 'hi');
});

test('Chinese is Simplified: Traditional falls through', () => {
  assert.equal(matchLocale(['zh-CN']), 'zh');
  assert.equal(matchLocale(['zh-Hans-SG']), 'zh');
  assert.equal(matchLocale(['zh']), 'zh');
  assert.equal(matchLocale(['zh-TW']), 'en');
  assert.equal(matchLocale(['zh-HK', 'zh-Hant']), 'en');
  assert.equal(matchLocale(['zh-TW', 'ja']), 'ja');
});

test('every code it can return is a supported language', () => {
  const codes = new Set(SUPPORTED.map((l) => l.code));
  for (const tag of ['en', 'es', 'fr', 'de', 'pt', 'ru', 'zh', 'ja', 'hi', 'ar']) {
    assert.ok(codes.has(matchLocale([tag])), tag);
  }
});
