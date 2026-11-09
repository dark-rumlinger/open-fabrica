/**
 * Tests for the shared prompt composer.
 *
 * slugifyDomain once existed in two byte-identical copies (the new-project
 * modal and the setup wizard); these pin the truncation rule that made it
 * non-obvious - it backs off to the last WHOLE token that fits, so a long
 * prompt-derived name doesn't become a run-on slug.
 *
 * validate() covers the two rules the submit path depends on: a blank name is
 * fine, a blank everything is not, and you cannot build without a model.
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
    this.dataset = {};
    this.listeners = {};
    this.innerHTML = '';
    this.value = '';
    this.textContent = '';
    const classes = new Set();
    this.classList = {
      add: (c) => { classes.add(c); this.className = [...classes].join(' '); },
      remove: (c) => { classes.delete(c); this.className = [...classes].join(' '); },
      contains: (c) => classes.has(c),
      toggle: (c, force) => {
        const on = force === undefined ? !classes.has(c) : !!force;
        if (on) classes.add(c); else classes.delete(c);
        this.className = [...classes].join(' ');
        return on;
      },
    };
  }
  setAttribute(k, v) { this.attributes[k] = String(v); }
  getAttribute(k) { return this.attributes[k]; }
  addEventListener(evt, fn) { (this.listeners[evt] ||= []).push(fn); }
  appendChild(child) { this.children.push(child); return child; }
  fire(evt, e = {}) {
    for (const fn of this.listeners[evt] || []) fn({ preventDefault() {}, ...e });
  }
}

globalThis.Node = StubNode;
globalThis.document = {
  createElement: (tag) => new StubNode(tag),
  createTextNode: (t) => new StubNode(`#text:${t}`),
};
globalThis.localStorage = { getItem: () => null, setItem: () => {} };
// The composer asks for the provider catalog. An empty one is a real state
// (no provider configured yet) and keeps these tests off the network.
globalThis.fetch = async () => ({ status: 200, ok: true, text: async () => '[]' });

const { slugifyDomain, createPromptComposer } = await import('../admin/app/ui/prompt-composer.js');

test('slugifyDomain: nothing to slugify yields an empty string', () => {
  assert.equal(slugifyDomain(''), '');
  assert.equal(slugifyDomain('   '), '');
  assert.equal(slugifyDomain('---!!!___'), '');
  assert.equal(slugifyDomain(null), '');
  assert.equal(slugifyDomain(undefined), '');
});

test('slugifyDomain: a single token becomes one label', () => {
  assert.equal(slugifyDomain('Portfolio'), 'portfolio.localhost');
  assert.equal(slugifyDomain('  Spaced  '), 'spaced.localhost');
});

test('slugifyDomain: at most three leading tokens survive', () => {
  assert.equal(slugifyDomain('one two three four five'), 'one-two-three.localhost');
  assert.equal(slugifyDomain("Bob's Coffee Shop Board"), 'bob-s-coffee.localhost');
});

test('slugifyDomain: over-long slugs truncate at a token boundary', () => {
  // "a-modern-landingpageforsaas" is 27 chars, under the 30 cap: kept whole.
  assert.equal(slugifyDomain('a modern landingpageforsaas'), 'a-modern-landingpageforsaas.localhost');
  // Adding a long third token pushes past 30, so the whole token is dropped
  // rather than sliced mid-word.
  assert.equal(
    slugifyDomain('a modern landingpageformysaasproduct'),
    'a-modern.localhost',
  );
});

test('slugifyDomain: an unsplittable over-long token is sliced as a last resort', () => {
  // No hyphen to back off to, so the cap wins over the token boundary.
  assert.equal(slugifyDomain('a'.repeat(40)), 'a'.repeat(30) + '.localhost');
});

test('validate: wants a prompt or a name first, then a model', async () => {
  const composer = await createPromptComposer();
  assert.equal(composer.validate(), 'need-prompt');

  // With an empty catalog there is no model to select, so the prompt rule
  // clearing exposes the model rule underneath it.
  composer.descInput.value = 'a file sharing site';
  assert.equal(composer.validate(), 'need-model', 'a prompt alone is not enough');

  composer.descInput.value = '';
  composer.nameInput.value = 'Crimson Otter';
  assert.equal(composer.validate(), 'need-model', 'a name alone is not enough');
});

test('validate: whitespace is not a prompt', async () => {
  const composer = await createPromptComposer();
  composer.descInput.value = '   \n  ';
  assert.equal(composer.validate(), 'need-prompt');
});

test('the name field drives the domain until the domain is touched', async () => {
  const composer = await createPromptComposer();

  composer.nameInput.value = 'Crimson Otter';
  composer.nameInput.fire('input');
  assert.equal(composer.domainInput.value, 'crimson-otter.localhost');

  composer.domainInput.fire('input'); // user takes over
  composer.nameInput.value = 'Teal Heron';
  composer.nameInput.fire('input');
  assert.equal(composer.domainInput.value, 'crimson-otter.localhost', 'a hand-edited domain is left alone');
});


test('user-flow tests: off by default (experimental), and the last choice is remembered like the detail level', async () => {
  const saved = {};
  const original = globalThis.localStorage;
  globalThis.localStorage = {
    getItem: (k) => (k in saved ? saved[k] : null),
    setItem: (k, v) => { saved[k] = v; },
  };
  try {
    const first = await createPromptComposer();
    assert.equal(first.userFlowsCheck.attributes.checked, undefined, 'off when nothing was chosen yet');

    // Turning it on is stored, and the next composer starts on.
    first.userFlowsCheck.checked = true;
    first.userFlowsCheck.fire('change');
    assert.equal(saved['of:userFlows'], 'on');
    const second = await createPromptComposer();
    assert.equal(second.userFlowsCheck.attributes.checked, '', 'on after it was turned on');

    // Turning it back off is stored too.
    second.userFlowsCheck.checked = false;
    second.userFlowsCheck.fire('change');
    assert.equal(saved['of:userFlows'], 'off');
    const third = await createPromptComposer();
    assert.equal(third.userFlowsCheck.attributes.checked, undefined);
  } finally {
    globalThis.localStorage = original;
  }
});

function findByClass(node, cls) {
  if (!node || typeof node !== 'object') return null;
  if (String(node.className || '').split(' ').includes(cls)) return node;
  for (const child of node.children || []) {
    const hit = findByClass(child, cls);
    if (hit) return hit;
  }
  return null;
}

test('advanced fold: the closed toggle summarises the current choices, and the open one does not', async () => {
  const original = globalThis.localStorage;
  globalThis.localStorage = {
    getItem: (k) => ({ 'of:ambitionLevel': 'low', 'of:userFlows': 'off' }[k] ?? null),
    setItem: () => {},
  };
  try {
    const composer = await createPromptComposer();
    const summary = findByClass(composer.element, 'advanced-toggle__summary');
    assert.equal(summary.textContent, 'Low detail · Tests off');
    composer.openAdvanced();
    assert.equal(summary.textContent, '', 'nothing to summarise while the fold is open');
  } finally {
    globalThis.localStorage = original;
  }
});

test('motes: decorative dots, hidden from assistive tech, each with its own timing', async () => {
  const { createMotes } = await import('../admin/app/ui/motes.js');
  const layer = createMotes(14);
  assert.equal(layer.className, 'home-motes');
  assert.equal(layer.attributes['aria-hidden'], 'true');
  assert.equal(layer.children.length, 14);
  const tiers = new Set(layer.children.map((m) => m.className.split(' ')[1]));
  assert.equal(tiers.size, 3, 'small, medium and large dots');
  const durations = new Set(layer.children.map((m) => m.style.animationDuration));
  assert.ok(durations.size > 1, 'dots must not all move in lockstep');
  for (const m of layer.children) {
    assert.match(m.style.animationDelay, /^-/, 'starts part-way up');
  }
});
