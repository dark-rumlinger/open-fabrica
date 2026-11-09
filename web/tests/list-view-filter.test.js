/**
 * Tests for the createListView in-view filter helpers: the default match
 * predicate and the filter-option normalizer. The DOM-driven parts of
 * createListView are exercised by the module-load smoke test and in the browser;
 * here we pin the pure logic that decides what a filter shows.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`).
 */

import test from 'node:test';
import assert from 'node:assert/strict';

// list-view.js imports modules with top-level browser references; the smallest
// stub that lets the graph load (nothing is rendered here).
class StubNode {
  constructor() {
    this.style = {};
    this.classList = { add() {}, remove() {}, toggle() {}, contains: () => false };
  }
  setAttribute() {}
  addEventListener() {}
  appendChild(c) { return c; }
  querySelector() { return null; }
}
globalThis.document = globalThis.document || {
  createElement: () => new StubNode(),
  createTextNode: () => new StubNode(),
};
globalThis.localStorage = globalThis.localStorage || { getItem: () => null, setItem() {}, removeItem() {} };

const { defaultPredicate, normalizeFilter } = await import('../admin/app/ui/list-view.js');

test('defaultPredicate matches string and number fields, case-insensitively', () => {
  const item = { id: 42, name: 'Alice', role: 'Admin', active: true };
  assert.equal(defaultPredicate(item, 'ali'), true, 'matches a string field');
  assert.equal(defaultPredicate(item, '42'), true, 'matches a number field');
  assert.equal(defaultPredicate(item, 'admin'), true, 'match is case-insensitive');
  assert.equal(defaultPredicate(item, 'zzz'), false, 'no false positive');
});

test('defaultPredicate ignores non-string/number fields', () => {
  assert.equal(defaultPredicate({ active: true, meta: { x: 'secret' } }, 'secret'), false);
  assert.equal(defaultPredicate({ active: true }, 'true'), false);
});

test('defaultPredicate handles primitives and null', () => {
  assert.equal(defaultPredicate('hello world', 'world'), true);
  assert.equal(defaultPredicate(null, 'x'), false);
});

test('normalizeFilter returns null when the option is off', () => {
  assert.equal(normalizeFilter(false), null);
  assert.equal(normalizeFilter(undefined), null);
});

test('normalizeFilter(true) uses the default placeholder and predicate', () => {
  const cfg = normalizeFilter(true);
  assert.equal(typeof cfg.placeholder, 'string');
  assert.ok(cfg.placeholder.length > 0);
  assert.equal(cfg.predicate, defaultPredicate);
});

test('normalizeFilter passes a custom placeholder and predicate through', () => {
  const custom = () => true;
  const cfg = normalizeFilter({ placeholder: 'Find user', predicate: custom });
  assert.equal(cfg.placeholder, 'Find user');
  assert.equal(cfg.predicate, custom);
});
