/**
 * Tests for the theme module's default.
 *
 * With no saved choice the theme is dark, whatever the operating system prefers;
 * a saved choice always wins. The un-attributed document is light, so
 * get() with no attribute matters as much as init() does. These also cover storage
 * being unavailable, which must not throw out of boot().
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`). See markdown.test.js for
 * why these live outside web/admin.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

// Smallest surface theme.js touches: one element's attribute map, storage, and the
// system colour-scheme query (os: 'dark' | 'light', or null for a browser without it).
function stubEnv({ storage = new Map(), throwing = false, os = 'light' } = {}) {
  const attrs = new Map();
  const listeners = [];
  globalThis.document = {
    documentElement: {
      setAttribute: (k, v) => attrs.set(k, String(v)),
      getAttribute: (k) => (attrs.has(k) ? attrs.get(k) : null),
    },
  };
  globalThis.localStorage = {
    getItem: (k) => { if (throwing) throw new Error('storage off'); return storage.has(k) ? storage.get(k) : null; },
    setItem: (k, v) => { if (throwing) throw new Error('storage off'); storage.set(k, String(v)); },
  };
  if (os === null) {
    delete globalThis.window;
  } else {
    globalThis.window = {
      matchMedia: () => ({ matches: os === 'dark', addEventListener: (_, fn) => listeners.push(fn) }),
    };
  }
  return { attrs, storage, osChanges: (dark) => listeners.forEach((fn) => fn({ matches: dark })) };
}

const theme = await import('../admin/app/core/theme.js');

test('get() with no attribute reports light', () => {
  stubEnv();
  assert.equal(theme.get(), 'light');
});

test('init() with nothing saved is dark whatever the system prefers, and does not save it', () => {
  for (const os of ['light', 'dark', null]) {
    const { attrs, storage } = stubEnv({ os });
    theme.init();
    assert.equal(attrs.get('data-theme'), 'dark');
    assert.equal(storage.has(theme.THEME_KEY), false);
  }
});

test('a saved choice beats the default, in both directions', () => {
  let env = stubEnv({ storage: new Map([[theme.THEME_KEY, 'light']]), os: 'dark' });
  theme.init();
  assert.equal(env.attrs.get('data-theme'), 'light');
  env = stubEnv({ storage: new Map([[theme.THEME_KEY, 'dark']]), os: 'light' });
  theme.init();
  assert.equal(env.attrs.get('data-theme'), 'dark');
});

test('toggle() round-trips from the default and saves the choice', () => {
  const { attrs, storage } = stubEnv();
  theme.init();
  assert.equal(theme.toggle(), 'light');
  assert.equal(attrs.get('data-theme'), 'light');
  assert.equal(storage.get(theme.THEME_KEY), 'light');
  assert.equal(theme.toggle(), 'dark');
});

// Private-browsing and blocked-cookie profiles throw on every storage access.
// The theme must still apply; it just doesn't persist.
test('unavailable storage does not break init() or set()', () => {
  const { attrs } = stubEnv({ throwing: true, os: 'dark' });
  assert.doesNotThrow(() => theme.init());
  assert.equal(attrs.get('data-theme'), 'dark');
  assert.doesNotThrow(() => theme.set('light'));
  assert.equal(attrs.get('data-theme'), 'light');
});
