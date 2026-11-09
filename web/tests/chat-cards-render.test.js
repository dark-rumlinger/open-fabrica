/**
 * Smoke test: every chat message/card renderer actually renders without throwing.
 *
 * The card renderers live across message.js and cards/*.js and share helpers like
 * timestamp(). import-hygiene/modules-load only check that modules LOAD; a helper
 * referenced inside a function body (not at module top level) stays undefined until
 * the function is CALLED. This test calls each renderer so a missing cross-module
 * reference (e.g. a card using timestamp() without importing it) fails the build
 * instead of surfacing as a runtime ReferenceError in the browser.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`). The DOM stub is
 * the smallest surface core/dom.js's h() touches - no jsdom.
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
    this.textContent = '';
    this.value = '';
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
}

globalThis.Node = StubNode;
globalThis.document = {
  createElement: (tag) => new StubNode(tag),
  createTextNode: (t) => new StubNode(`#text:${t}`),
};
// Minimal window: revealProgress reads window.matchMedia and falls back to the
// final width when IntersectionObserver is absent (it is, under node).
globalThis.window = {};

const m = await import('../admin/app/ui/chat/message.js');

const AT = '2026-01-01T00:00:00Z';

// Each case renders one exported renderer with representative args. A throw (e.g. a
// card calling a helper it never imported) fails the test with the renderer's name.
const cases = {
  createUserMessage: () => m.createUserMessage('hi there', AT),
  createAssistantMessage: () => m.createAssistantMessage('a reply', AT),
  createStreamingMessage: () => m.createStreamingMessage(),
  createAutomatedMessage: () => m.createAutomatedMessage('an automated note', AT),
  createBrainMessage: () => m.createBrainMessage('thinking it through', AT),
  createStageTransition: () => m.createStageTransition('build', 1000),
  createWelcomeBack: () => m.createWelcomeBack('Mark'),
  createMilestone: () => m.createMilestone('shipped'),
  createBuildProgressCard: () => m.createBuildProgressCard({
    completed: ['task one'], doneCount: 1, totalCount: 3, remaining: ['task two', 'task three'], createdAt: AT,
  }),
  createBuildPlanCard: () => m.createBuildPlanCard({ tasks: ['Build the page', 'Add the form'], createdAt: AT }),
  createToolCall: () => m.createToolCall('manage_files', 'done', '{"success":true}', { action: 'save', path: 'index.html' }),
};

for (const [name, render] of Object.entries(cases)) {
  test(`${name} renders without throwing`, () => {
    let el;
    assert.doesNotThrow(() => { el = render(); }, `${name} threw while rendering`);
    assert.ok(el, `${name} returned nothing`);
  });
}
