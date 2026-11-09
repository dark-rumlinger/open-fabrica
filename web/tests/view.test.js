/**
 * Tests for the view controller contract.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`). See markdown.test.js for
 * why these live outside web/admin.
 *
 * A mount may be sync or async, and the contract has to hold for both: a destroy
 * returned from an async mount arrives on a Promise, and dropping it leaks
 * whatever it was meant to clean up. The scope cleanups run either way, so a
 * missed destroy surfaces only as a stale listener - hence the explicit cases.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { defineView } from '../admin/app/core/view.js';

test('sync mount: returned destroy runs, and scope cleanups run', () => {
  const calls = [];
  const view = defineView((ctx) => {
    ctx.scope.add(() => calls.push('scope'));
    return { destroy: () => calls.push('destroy') };
  })();
  view.destroy();
  assert.deepEqual(calls, ['destroy', 'scope']);
});

test('sync mount returning nothing still disposes the scope', () => {
  const calls = [];
  const view = defineView((ctx) => {
    ctx.scope.add(() => calls.push('scope'));
  })();
  view.destroy();
  assert.deepEqual(calls, ['scope']);
});

test('async mount: returned destroy is not dropped', async () => {
  const calls = [];
  const view = defineView(async (ctx) => {
    ctx.scope.add(() => calls.push('scope'));
    await Promise.resolve();
    return { destroy: () => calls.push('destroy') };
  })();

  // Let the mount settle, as the router does by simply staying on the route.
  await new Promise((r) => setImmediate(r));
  view.destroy();
  assert.deepEqual(calls, ['destroy', 'scope'], 'async destroy was dropped');
});

test('async mount: destroy before the mount settles still tears the view down', async () => {
  const calls = [];
  let release;
  const gate = new Promise((r) => { release = r; });

  const view = defineView(async (ctx) => {
    ctx.scope.add(() => calls.push('scope'));
    await gate;
    return { destroy: () => calls.push('destroy') };
  })();

  // Route changed before the mount finished: the router destroys immediately.
  view.destroy();
  assert.deepEqual(calls, ['scope'], 'scope must dispose without waiting for the mount');

  release();
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(calls, ['scope', 'destroy'], 'late controller must be torn down, not leaked');
});

test('async mount: no destroy returned still disposes the scope', async () => {
  const calls = [];
  const view = defineView(async (ctx) => {
    ctx.scope.add(() => calls.push('scope'));
    await Promise.resolve();
  })();
  await new Promise((r) => setImmediate(r));
  view.destroy();
  assert.deepEqual(calls, ['scope']);
});

test('a throwing destroy does not prevent scope disposal', () => {
  const calls = [];
  const view = defineView((ctx) => {
    ctx.scope.add(() => calls.push('scope'));
    return { destroy: () => { throw new Error('boom'); } };
  })();
  view.destroy();
  assert.deepEqual(calls, ['scope'], 'scope must dispose even if the view destroy throws');
});
