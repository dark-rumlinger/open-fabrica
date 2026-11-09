/**
 * Tests for the chat input's send/stop button.
 *
 * The button is one control with two jobs, and the state that decides which is
 * a SET of reasons: a chat turn and the build pipeline can be in flight at the
 * same time, so whichever finishes first must not clear the other's stop. These
 * cover that union, the dispatch (send vs stop), and the Enter suppression.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`). See markdown.test.js for
 * why these live outside web/admin. The DOM stub below is the smallest surface
 * core/dom.js's h() actually touches - no jsdom, nothing to install.
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
    this.scrollHeight = 72;
    const classes = new Set();
    this.classList = {
      add: (c) => { classes.add(c); this.className = [...classes].join(' '); },
      remove: (c) => { classes.delete(c); this.className = [...classes].join(' '); },
      contains: (c) => classes.has(c),
    };
  }
  setAttribute(k, v) { this.attributes[k] = String(v); }
  getAttribute(k) { return this.attributes[k]; }
  addEventListener(evt, fn) { (this.listeners[evt] ||= []).push(fn); }
  appendChild(child) { this.children.push(child); return child; }
  // Test helper: fire a listener the way the browser would.
  fire(evt, e = {}) {
    for (const fn of this.listeners[evt] || []) fn({ preventDefault() {}, ...e });
  }
}

globalThis.Node = StubNode;
globalThis.document = {
  createElement: (tag) => new StubNode(tag),
  createTextNode: (t) => new StubNode(`#text:${t}`),
};

const { createChatInput } = await import('../admin/app/ui/chat/input.js');

// The button is the second child of the wrapper, which is the input's only child.
function partsOf(input) {
  const wrapper = input.element.children[0];
  return { textarea: wrapper.children[0], button: wrapper.children[1] };
}

function newInput() {
  const sent = [];
  const stops = [];
  const input = createChatInput((t) => sent.push(t), () => stops.push(true));
  return { input, sent, stops, ...partsOf(input) };
}

test('idle: the button sends, and is disabled while the textarea is empty', () => {
  const { input, sent, textarea, button } = newInput();

  input.setWorking('engine', false); // force a render without any work in flight
  assert.equal(button.disabled, true, 'empty textarea should disable send');
  assert.equal(button.classList.contains('chat-input__send--stop'), false);

  textarea.value = 'ship it';
  textarea.fire('input');
  assert.equal(button.disabled, false);

  button.fire('click');
  assert.deepEqual(sent, ['ship it']);
  assert.equal(textarea.value, '', 'sending clears the textarea');
});

test('working: the button becomes stop and dispatches to onStop', () => {
  const { input, sent, stops, textarea, button } = newInput();

  input.setWorking('engine', true);
  assert.equal(button.classList.contains('chat-input__send--stop'), true);
  assert.equal(button.disabled, false, 'stop stays clickable with an empty textarea');
  assert.equal(button.getAttribute('aria-label'), 'Stop');

  textarea.value = 'a message being composed';
  button.fire('click');
  assert.deepEqual(stops, [true], 'click should stop, not send');
  assert.deepEqual(sent, [], 'nothing should have been sent');
  assert.equal(textarea.value, 'a message being composed', 'stopping keeps the draft');
});

test('two reasons: the first to finish does not clear the second', () => {
  const { input, button } = newInput();

  input.setWorking('engine', true);   // a build is running
  input.setWorking('chat', true);     // and the owner sent a chat turn
  assert.equal(input.isWorking(), true);

  input.setWorking('chat', false);    // the chat turn comes back
  assert.equal(input.isWorking(), true, 'the build is still running');
  assert.equal(button.classList.contains('chat-input__send--stop'), true);

  input.setWorking('engine', false);  // the build stops too
  assert.equal(input.isWorking(), false);
  assert.equal(button.classList.contains('chat-input__send--stop'), false);
});

test('a redundant clear is not a toggle', () => {
  const { input } = newInput();
  input.setWorking('engine', true);
  input.setWorking('chat', false);
  input.setWorking('chat', false);
  assert.equal(input.isWorking(), true);
});

test('stopping: the button holds and cannot be clicked twice', () => {
  const { input, stops, button } = newInput();

  input.setWorking('engine', true);
  input.setStopping(true);
  assert.equal(button.disabled, true);
  assert.equal(button.getAttribute('aria-label'), 'Stopping…');
  // The hold is readable, and only from here: the stopper asks this rather than
  // keeping its own copy, which nothing would be left to release.
  assert.equal(input.isStopping(), true);

  input.setStopping(false);
  assert.equal(button.disabled, false);
  assert.equal(input.isStopping(), false);
  button.fire('click');
  assert.deepEqual(stops, [true]);
});

test('Enter sends when idle and never stops when working', () => {
  const { input, sent, stops, textarea } = newInput();

  textarea.value = 'first';
  textarea.fire('keydown', { key: 'Enter', shiftKey: false });
  assert.deepEqual(sent, ['first']);

  input.setWorking('engine', true);
  textarea.value = 'second';
  textarea.fire('keydown', { key: 'Enter', shiftKey: false });
  assert.deepEqual(sent, ['first'], 'Enter must not send while work is in flight');
  assert.deepEqual(stops, [], 'Enter must not stop either - a stop is deliberate');

  // Shift+Enter is still a newline, never a send.
  input.setWorking('engine', false);
  textarea.fire('keydown', { key: 'Enter', shiftKey: true });
  assert.deepEqual(sent, ['first']);
});

test('without an onStop callback the button never enters stop mode', () => {
  const sent = [];
  const input = createChatInput((t) => sent.push(t));
  const { textarea, button } = partsOf(input);

  input.setWorking('engine', true);
  assert.equal(input.isWorking(), false);
  assert.equal(button.classList.contains('chat-input__send--stop'), false);

  textarea.value = 'still sends';
  button.fire('click');
  assert.deepEqual(sent, ['still sends']);
});
