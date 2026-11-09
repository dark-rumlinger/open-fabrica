/**
 * Tests for core/dom.js h().
 *
 * h() once passed null/undefined straight to setAttribute, which writes the
 * strings "null"/"undefined". For aria-current that is read as "true", so a
 * screen reader announced every tab as the current page. These pin the
 * not-set behaviour and the ARIA boolean handling.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`). See markdown.test.js for
 * why these live outside web/admin. The DOM stub is the smallest surface h() touches.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

class StubNode {
  constructor(tag) {
    this.tagName = tag;
    this.className = '';
    this.style = {};
    this.children = [];
    this.attributes = {};
    this.dataset = {};
    this.listeners = {};
  }
  setAttribute(k, v) { this.attributes[k] = String(v); }
  getAttribute(k) { return this.attributes[k]; }
  addEventListener(evt, fn) { (this.listeners[evt] ||= []).push(fn); }
  appendChild(child) { this.children.push(child); return child; }
}

globalThis.Node = StubNode;
globalThis.document = {
  createElement: (tag) => new StubNode(tag),
  createTextNode: (t) => new StubNode(`#text:${t}`),
};

const { h } = await import('../admin/app/core/dom.js');

test('h: null and undefined attributes are not written', () => {
  const el = h('a', { 'aria-current': null, title: undefined, href: '/x' });
  assert.equal('aria-current' in el.attributes, false);
  assert.equal('title' in el.attributes, false);
  assert.equal(el.attributes.href, '/x');
});

test('h: a real aria-current value is kept', () => {
  const el = h('a', { 'aria-current': 'page' });
  assert.equal(el.attributes['aria-current'], 'page');
});

test('h: aria booleans become "true"/"false" strings, including false', () => {
  const el = h('button', { 'aria-expanded': false, 'aria-pressed': true });
  assert.equal(el.attributes['aria-expanded'], 'false');
  assert.equal(el.attributes['aria-pressed'], 'true');
});

test('h: plain boolean attributes keep presence semantics', () => {
  const on = h('button', { disabled: true });
  const off = h('button', { disabled: false });
  assert.equal(on.attributes.disabled, '');
  assert.equal('disabled' in off.attributes, false);
});
