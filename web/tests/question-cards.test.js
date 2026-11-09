/**
 * Regression test for the grouped chat question card: a secret answer must be
 * masked as "(hidden)" in the answered summary, never echoed back in plaintext.
 *
 * The single chat card and the panel grouped form both mask secrets; the chat
 * group once forgot to flag its items as secret, so the raw value (an API key or
 * password) was rendered into the DOM on submit. This drives the real
 * createQuestionGroup through a submit and inspects the rendered summary.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`). The DOM stub
 * is the smallest surface core/dom.js's h() and the toast helper touch, no jsdom.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

// Toast auto-dismiss schedules a 4s timer; unref it so a pending timer can't keep
// the test runner alive after the assertions finish.
const realSetTimeout = globalThis.setTimeout;
globalThis.setTimeout = (fn, ms, ...rest) => {
  const handle = realSetTimeout(fn, ms, ...rest);
  if (handle && typeof handle.unref === 'function') handle.unref();
  return handle;
};

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
    this.dataset = {};
    this.parentNode = null;
    const classes = new Set();
    this.classList = {
      add: (c) => { classes.add(c); },
      remove: (c) => { classes.delete(c); },
      contains: (c) => classes.has(c),
      toggle: (c, force) => {
        const on = force === undefined ? !classes.has(c) : force;
        if (on) classes.add(c); else classes.delete(c);
        return on;
      },
    };
  }
  setAttribute(k, v) { this.attributes[k] = String(v); }
  addEventListener(evt, fn) { (this.listeners[evt] ||= []).push(fn); }
  removeChild(child) {
    const i = this.children.indexOf(child);
    if (i !== -1) this.children.splice(i, 1);
    child.parentNode = null;
  }
  appendChild(child) { this.children.push(child); child.parentNode = this; return child; }
  focus() {}
  scrollIntoView() {}
  set textContent(v) {
    this.children = [];
    if (v !== '' && v !== null && v !== undefined) this.appendChild(new StubNode(`#text:${v}`));
  }
  get textContent() { return this.text; }
  get firstChild() { return this.children[0] || null; }
  get text() {
    return this.children.map((c) => (typeof c.tagName === 'string' && c.tagName.startsWith('#text:')
      ? c.tagName.slice('#text:'.length)
      : c.text || '')).join('');
  }
}

globalThis.Node = StubNode;
globalThis.document = {
  body: new StubNode('body'),
  createElement: (tag) => new StubNode(tag),
  createTextNode: (t) => new StubNode(`#text:${t}`),
  dispatchEvent: () => {},
};
globalThis.CustomEvent = class { constructor(type, init) { this.type = type; this.detail = init?.detail; } };
globalThis.requestAnimationFrame = () => {};
globalThis.localStorage = { getItem: () => null, setItem: () => {}, removeItem: () => {} };
globalThis.sessionStorage = { getItem: () => null, setItem: () => {}, removeItem: () => {} };
globalThis.fetch = async () => ({ ok: true, status: 200, text: async () => '{}' });

const { createQuestionGroup } = await import('../admin/app/ui/question-cards.js');

function find(node, pred, out = []) {
  if (pred(node)) out.push(node);
  for (const c of node.children || []) find(c, pred, out);
  return out;
}
const byClass = (el, cls) => find(el, (n) => typeof n.className === 'string' && n.className.split(' ').includes(cls));

test('a secret answer is masked, a normal answer is shown, in the grouped card summary', async () => {
  const group = createQuestionGroup([
    { id: 1, type: 'secret', secret_name: 'STRIPE_KEY', question: 'Stripe secret key?' },
    { id: 2, type: 'text', question: 'App name?' },
  ]);

  const inputs = find(group.element, (n) => n.tagName === 'input');
  assert.equal(inputs.length, 2, 'one input per question');
  inputs[0].value = 'sk_live_SUPERSECRET';
  inputs[1].value = 'My App';

  // Invoke the Submit-all handler directly so we can await the async submit.
  const submitBtn = find(group.element, (n) => n.tagName === 'button' && n.text === 'Submit all')[0];
  assert.ok(submitBtn, 'submit button exists');
  await submitBtn.listeners.click[0]();

  const answered = byClass(group.element, 'question-group__answered-a');
  assert.equal(answered.length, 2, 'two answered rows rendered');
  assert.equal(answered[0].text, '(hidden)', 'secret answer must be masked');
  assert.equal(answered[1].text, 'My App', 'normal answer is shown verbatim');

  // Belt and suspenders: the raw secret must appear nowhere in the summary DOM.
  const summary = byClass(group.element, 'question-group__answered')[0];
  assert.ok(!summary.text.includes('sk_live_SUPERSECRET'), 'raw secret must not be in the DOM');
});
