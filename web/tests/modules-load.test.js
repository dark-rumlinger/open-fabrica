/**
 * Smoke test: every module under admin/app must import cleanly.
 *
 * ES module linking fails the whole import when a file asks for a name another
 * module does not export (a renamed helper, a typo in an import list). The
 * browser reports that only for the page that loads the module, so a broken
 * import in a rarely opened view can ship unnoticed. Importing every file here
 * turns it into a test failure instead.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`). See markdown.test.js for
 * why these live outside web/admin. The globals below are the smallest set that
 * lets modules with top-level browser references load; nothing is rendered.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readdirSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

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
globalThis.Image = class { addEventListener() {} };
globalThis.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
globalThis.sessionStorage = { getItem: () => null, setItem() {}, removeItem() {} };

const appDir = fileURLToPath(new URL('../admin/app/', import.meta.url));

function walk(dir) {
  return readdirSync(dir).flatMap((name) => {
    const full = join(dir, name);
    return statSync(full).isDirectory() ? walk(full) : full.endsWith('.js') ? [full] : [];
  });
}

// main.js boots the whole app on import and theme-boot.js is a classic script
// for index.html; neither is a library module.
const SKIP = new Set(['main.js', 'theme-boot.js']);

// The games and their registry import each other. The app always enters through
// the registry (via games/host.js); entering through a game module first hits a
// temporal-dead-zone error that is an artifact of import order, not a bug in
// the app. Load the registry first, as the app does.
await import(pathToFileURL(join(appDir, 'ui/chat/games/registry.js')).href);

for (const file of walk(appDir)) {
  const rel = relative(appDir, file).replace(/\\/g, '/');
  if (SKIP.has(rel)) continue;
  test(`imports cleanly: ${rel}`, async () => {
    await assert.doesNotReject(import(pathToFileURL(file).href));
  });
}
