/**
 * Tests for the assistant language setting: the interface language rides on every
 * API request, and the Account dialog offers the server's vocabulary of choices.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`). See markdown.test.js for
 * why these live outside web/admin.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

class StubNode {
  constructor() {
    this.style = {};
    this.dataset = {};
    this.children = [];
    this.classList = { add() {}, remove() {}, toggle() {}, contains: () => false };
  }
  setAttribute() {}
  getAttribute() { return null; }
  addEventListener() {}
  removeEventListener() {}
  appendChild(c) { return c; }
  querySelector() { return null; }
  querySelectorAll() { return []; }
}

globalThis.Node = StubNode;
globalThis.HTMLElement = StubNode;
globalThis.document = {
  createElement: () => new StubNode(),
  createTextNode: () => new StubNode(),
  getElementById: () => null,
  addEventListener() {},
  removeEventListener() {},
  documentElement: new StubNode(),
  body: new StubNode(),
  readyState: 'complete',
};
globalThis.window = {
  addEventListener() {},
  removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {}, addListener() {} }),
  location: { pathname: '/', origin: 'http://localhost', href: '' },
  history: { pushState() {}, replaceState() {} },
};
globalThis.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
globalThis.sessionStorage = { getItem: () => null, setItem() {}, removeItem() {} };

const { get, post, rawFetch, LOCALE_HEADER } = await import('../admin/app/core/http.js');
const { getLocale, SUPPORTED } = await import('../admin/app/core/i18n.js');
const { assistantLanguageOptions, selectableValue } = await import('../admin/app/ui/assistant-language.js');

/** Replaces fetch for one call and returns the headers it was given. */
async function headersSentBy(call) {
  let sent;
  globalThis.fetch = async (_url, opts) => {
    sent = opts.headers;
    return new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    await call();
  } finally {
    delete globalThis.fetch;
  }
  return sent;
}

test('every JSON request reports the interface language', async () => {
  assert.equal(LOCALE_HEADER, 'X-Of-Locale');
  for (const call of [() => get('/admin/api/x'), () => post('/admin/api/x', {})]) {
    const sent = await headersSentBy(call);
    assert.equal(sent[LOCALE_HEADER], getLocale());
  }
});

test('raw requests (streams, downloads) report it too', async () => {
  const sent = await headersSentBy(() => rawFetch('/admin/api/x'));
  assert.equal(sent[LOCALE_HEADER], getLocale());
});

test('a caller header is kept alongside the locale header', async () => {
  const sent = await headersSentBy(() => get('/admin/api/x', { headers: { 'X-Setup-Token': 'abc' } }));
  assert.equal(sent['X-Setup-Token'], 'abc');
  assert.equal(sent[LOCALE_HEADER], getLocale());
});

test('the header name matches the one the server reads', () => {
  const goSource = readFileSync(
    fileURLToPath(new URL('../../internal/replylang/replylang.go', import.meta.url)), 'utf8');
  const match = /const Header = "([^"]+)"/.exec(goSource);
  assert.ok(match, 'replylang.Header not found');
  assert.equal(LOCALE_HEADER, match[1]);
});

test('the assistant language options are the two relative choices then every interface language', () => {
  const options = assistantLanguageOptions();
  assert.deepEqual(options.slice(0, 2).map((o) => o.value), ['', 'match']);
  assert.deepEqual(options.slice(2).map((o) => o.value), SUPPORTED.map((l) => l.code));
  for (const o of options) assert.ok(o.label && o.label !== '', `option ${o.value} has no label`);
  assert.equal(new Set(options.map((o) => o.value)).size, options.length, 'duplicate option values');
});

test('selectableValue falls back to the default for a value with no option', () => {
  assert.equal(selectableValue(''), '');
  assert.equal(selectableValue('match'), 'match');
  assert.equal(selectableValue('es'), 'es');
  assert.equal(selectableValue('klingon'), '');
  assert.equal(selectableValue(undefined), '');
});
