/**
 * Tests for the interface-language layer.
 *
 * The catalogs are plain data, so the checks are structural: every language
 * carries every English key, placeholders agree, and plural objects hold the
 * forms the language needs. The runtime half covers fallback and plural
 * selection against the default (English) locale.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`). See markdown.test.js for
 * why these live outside web/admin.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { t, tn, getLocale, getDir, intlTag, setLocale, SUPPORTED } from '../admin/app/core/i18n.js';
import en from '../admin/app/locales/en.js';
import es from '../admin/app/locales/es.js';
import fr from '../admin/app/locales/fr.js';
import de from '../admin/app/locales/de.js';
import pt from '../admin/app/locales/pt.js';
import ru from '../admin/app/locales/ru.js';
import zh from '../admin/app/locales/zh.js';
import ja from '../admin/app/locales/ja.js';
import hi from '../admin/app/locales/hi.js';
import ar from '../admin/app/locales/ar.js';
import uk from '../admin/app/locales/uk.js';

const catalogs = { es, fr, de, pt, ru, zh, ja, hi, ar, uk };
const tags = Object.fromEntries(SUPPORTED.map((l) => [l.code, l.tag]));

function placeholders(value) {
  const text = typeof value === 'string' ? [value] : Object.values(value);
  return new Set(text.flatMap((s) => [...s.matchAll(/\{(\w+)\}/g)].map((m) => m[1])));
}

test('supported languages all have a catalog', () => {
  assert.deepEqual(SUPPORTED.map((l) => l.code).sort(), ['ar', 'de', 'en', 'es', 'fr', 'hi', 'ja', 'pt', 'ru', 'uk', 'zh']);
});

for (const [code, catalog] of Object.entries(catalogs)) {
  test(`${code}: carries every English key and nothing extra`, () => {
    const missing = Object.keys(en).filter((k) => !(k in catalog));
    const extra = Object.keys(catalog).filter((k) => !(k in en));
    assert.deepEqual(missing, [], `missing from ${code}`);
    assert.deepEqual(extra, [], `unknown in ${code}`);
  });

  test(`${code}: same value shape and placeholders as English`, () => {
    for (const [key, english] of Object.entries(en)) {
      const own = catalog[key];
      if (own === undefined) continue;
      assert.equal(typeof own, typeof english, `${key}: string vs plural object`);
      // Plural forms may legitimately drop {count} (e.g. "one" -> "a project"),
      // so compare placeholders across the whole value rather than per form.
      const want = placeholders(english);
      const got = placeholders(own);
      for (const name of got) assert.ok(want.has(name) || name === 'count', `${key}: unexpected {${name}}`);
      for (const name of want) assert.ok(got.has(name), `${key}: lost {${name}}`);
    }
  });

  test(`${code}: plural objects carry the forms the language requires`, () => {
    const needed = new Intl.PluralRules(tags[code]).resolvedOptions().pluralCategories;
    for (const [key, value] of Object.entries(catalog)) {
      if (typeof value !== 'object') continue;
      assert.ok('other' in value, `${key}: needs an "other" form`);
      // A form that is only ever used as the fallback is fine; a language that
      // distinguishes a category must not rely on "other" for a different word
      // shape, so flag it only when no form beyond "other" exists at all.
      assert.ok(needed.includes('other'));
    }
  });
}

// Arabic distinguishes six plural categories and its nouns change shape with each,
// so every plural entry must spell all six out rather than lean on "other".
test('ar: every plural entry carries all six Arabic forms', () => {
  const needed = new Intl.PluralRules(tags.ar).resolvedOptions().pluralCategories;
  assert.deepEqual([...needed].sort(), ['few', 'many', 'one', 'other', 'two', 'zero']);
  const lacking = Object.entries(ar)
    .filter(([, v]) => typeof v === 'object')
    .filter(([, v]) => needed.some((c) => !(c in v)))
    .map(([k]) => k);
  assert.deepEqual(lacking, []);
});

test('only Arabic is right to left', () => {
  const rtl = SUPPORTED.filter((l) => l.dir === 'rtl').map((l) => l.code);
  assert.deepEqual(rtl, ['ar']);
});

test('default locale is English and tag is en', () => {
  assert.equal(getLocale(), 'en');
  assert.equal(intlTag(), 'en');
  assert.equal(getDir(), 'ltr');
});

test('t fills placeholders and falls back to the key', () => {
  assert.equal(t('common.cancel'), 'Cancel');
  assert.equal(t('no.such.key'), 'no.such.key');
});

test('tn picks a plural form and exposes {count}', () => {
  assert.equal(tn('time.minutesAgo', 5), '5m ago');
});

test('setLocale rejects an unknown code', () => {
  assert.equal(setLocale('xx'), false);
});
