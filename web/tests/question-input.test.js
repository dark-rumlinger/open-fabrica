/**
 * Tests for the shared question input: which control a question renders and the
 * value it hands back. The owner answers the same questions from chat cards, the
 * grouped form and the questions panels, so this builder is the single place the
 * answer text is made.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`). The DOM stub is
 * the smallest surface core/dom.js's h() touches - no jsdom, nothing to install.
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
    this.listeners = {};
    this.innerHTML = '';
    this.value = '';
    const classes = new Set();
    this.classList = {
      add: (c) => { classes.add(c); },
      remove: (c) => { classes.delete(c); },
      contains: (c) => classes.has(c),
    };
  }
  setAttribute(k, v) { this.attributes[k] = String(v); }
  addEventListener(evt, fn) { (this.listeners[evt] ||= []).push(fn); }
  appendChild(child) { this.children.push(child); return child; }
  fire(evt) { for (const fn of this.listeners[evt] || []) fn({ preventDefault() {} }); }
  get text() {
    return this.children.map((c) => (typeof c.tagName === 'string' && c.tagName.startsWith('#text:')
      ? c.tagName.slice('#text:'.length)
      : c.text || '')).join('');
  }
}

globalThis.Node = StubNode;
globalThis.document = {
  createElement: (tag) => new StubNode(tag),
  createTextNode: (t) => new StubNode(`#text:${t}`),
};

const { buildQuestionInput, isMultiChoiceType } = await import('../admin/app/ui/question-input.js');

function find(node, pred, out = []) {
  if (pred(node)) out.push(node);
  for (const c of node.children || []) find(c, pred, out);
  return out;
}

const buttons = (el) => find(el, (n) => n.tagName === 'button');
const inputs = (el) => find(el, (n) => n.tagName === 'input');
const button = (el, label) => buttons(el).find((b) => b.text === label);

test('multi_choice returns every ticked option, joined the way the engine reads it', () => {
  const { inputEl, getValue, hasValue } = buildQuestionInput({
    type: 'multi_choice',
    options: '["Email","SMS","Push"]',
  });
  assert.equal(hasValue(), false);

  button(inputEl, 'Email').fire('click');
  button(inputEl, 'Push').fire('click');
  assert.equal(getValue(), 'Email, Push');
  assert.equal(hasValue(), true);

  button(inputEl, 'Email').fire('click'); // untick
  assert.equal(getValue(), 'Push');
});

test('single choice returns the tapped option', () => {
  const { inputEl, getValue } = buildQuestionInput({
    type: 'single_choice',
    options: ['Photo', 'Illustration'],
  });
  button(inputEl, 'Illustration').fire('click');
  assert.equal(getValue(), 'Illustration');
});

test('approval options stored as a plain comma list still become buttons', () => {
  const { inputEl } = buildQuestionInput({ type: 'approval', options: 'Approve, Deny' });
  assert.ok(button(inputEl, 'Approve'), 'Approve button');
  assert.ok(button(inputEl, 'Deny'), 'Deny button');
});

test('a secret question is always a password input, whatever options it carries', () => {
  const { inputEl, getValue } = buildQuestionInput({
    type: 'secret',
    secret_name: 'STRIPE_KEY',
    options: '["a","b"]',
  });
  const [field] = inputs(inputEl);
  assert.equal(field.attributes.type, 'password');
  assert.equal(buttons(inputEl).length, 0, 'no option buttons for a secret');
  field.value = ' sk_live_1 ';
  assert.equal(getValue(), 'sk_live_1');
});

test('a form returns its fields as one JSON object', () => {
  const { inputEl, getValue, hasValue } = buildQuestionInput({
    type: 'form',
    fields: '[{"name":"pk","label":"Publishable key"},{"name":"sk","label":"Secret","type":"secret"}]',
  });
  const [pk, sk] = inputs(inputEl);
  assert.equal(sk.attributes.type, 'password');
  assert.equal(hasValue(), false);
  pk.value = 'pk_1';
  assert.equal(hasValue(), true);
  assert.deepEqual(JSON.parse(getValue()), { pk: 'pk_1', sk: '' });
});

test('a form with broken field definitions cannot be submitted', () => {
  const { getValue, hasValue } = buildQuestionInput({ type: 'form', fields: 'not json' });
  assert.equal(hasValue(), false);
  assert.equal(getValue(), '');
});

test('isMultiChoiceType only matches multi_choice', () => {
  assert.equal(isMultiChoiceType('multi_choice'), true);
  assert.equal(isMultiChoiceType('single_choice'), false);
  assert.equal(isMultiChoiceType(undefined), false);
});
