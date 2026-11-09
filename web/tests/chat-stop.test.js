/**
 * Tests for the chat stop button's dispatcher.
 *
 * The interrupt endpoint only sets a flag and returns, so a 200 means "asked",
 * not "stopped": the engine is still winding up the turn in flight. That gap is
 * the whole reason the button holds at "Stopping…", and it is where this module
 * gets subtle. Two failures it has to stay clear of, both covered below:
 *
 *   - releasing the hold on the 200 puts a live Stop button back under the
 *     owner's cursor while tool calls keep streaming, which reads as a stop that
 *     did nothing;
 *   - keeping a second copy of the hold here would never see the events that
 *     release it, latching the re-entrancy guard and killing the button for good.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`). Same zero-dependency
 * approach as chat-input.test.js: stub only what the imports actually touch.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

// http.js reads the JWT at module scope on every request; toast.js builds DOM
// nodes only when a toast is actually shown.
globalThis.localStorage = { getItem: () => null, removeItem() {} };
globalThis.requestAnimationFrame = (fn) => fn();
globalThis.document = {
  body: { appendChild() {} },
  createElement: () => ({
    className: '',
    style: {},
    textContent: '',
    classList: { add() {}, remove() {} },
    setAttribute() {},
    addEventListener() {},
    appendChild() {},
    remove() {},
  }),
};

const { createStopper } = await import('../admin/app/views/project/chat/stop.js');

// fakeInput stands in for the real chat input: the hold and the working set live
// there, which is exactly the property these tests are pinning.
function fakeInput() {
  let stopping = false;
  const working = new Map();
  return {
    setStopping(v) { stopping = !!v; },
    isStopping: () => stopping,
    setWorking(reason, active) { working.set(reason, !!active); },
    working,
  };
}

// harness wires a stopper over a scripted fetch, capturing the calls each made.
function harness({ interruptStatus = 200, phase = { key: 'building', stalled: false } } = {}) {
  const calls = [];
  const timers = [];
  globalThis.fetch = async (url, opts) => {
    const method = opts?.method || 'GET';
    calls.push(`${method} ${url}`);
    if (url.endsWith('/interrupt')) {
      return {
        ok: interruptStatus < 400,
        status: interruptStatus,
        text: async () => JSON.stringify({ error: 'nothing is running for this project' }),
      };
    }
    return { ok: true, status: 200, text: async () => JSON.stringify({ id: 7, phase }) };
  };

  const chatInput = fakeInput();
  const sender = { sending: false, aborted: false, isSending() { return this.sending; }, abort() { this.aborted = true; } };
  // Mirrors core/lifecycle.js's scope.timeout: hand back a cancel, and let the
  // test fire the callback itself rather than waiting out a real minute.
  const scope = { timeout: (fn) => { timers.push(fn); return () => {}; } };

  const stopper = createStopper({ projectId: 7, chatInput, sender, scope });
  return { stopper, chatInput, sender, calls, timers };
}

test('a successful interrupt HOLDS the button: the engine has only been asked', async () => {
  const { stopper, chatInput, calls } = harness();

  stopper.stop();
  await new Promise((r) => setImmediate(r));

  assert.deepEqual(calls, ['POST /admin/api/engine/7/interrupt']);
  assert.equal(chatInput.isStopping(), true,
    'the hold must survive the 200 - the engine is still winding up the turn in flight');
});

test('a 409 releases the hold, re-seeds from the engine, and says so', async () => {
  const { stopper, chatInput, calls } = harness({
    interruptStatus: 409,
    phase: { key: 'live', stalled: false },
  });

  stopper.stop();
  await new Promise((r) => setImmediate(r));

  assert.equal(chatInput.isStopping(), false, 'nothing was running, so the hold must not stick');
  assert.deepEqual(calls, [
    'POST /admin/api/engine/7/interrupt',
    'GET /admin/api/projects/7',
  ], 'a stale button must resync from the engine rather than guess');
  assert.equal(chatInput.working.get('engine'), false, 'the re-seed should clear the stale working reason');
});

test('the button survives a landed stop: a second stop really asks again', async () => {
  const { stopper, chatInput, calls } = harness();

  stopper.stop();
  await new Promise((r) => setImmediate(r));
  // What the engine.stop_landed / project.phase_changed watchers do.
  chatInput.setStopping(false);

  stopper.stop();
  await new Promise((r) => setImmediate(r));

  assert.deepEqual(calls, [
    'POST /admin/api/engine/7/interrupt',
    'POST /admin/api/engine/7/interrupt',
  ], 'the stop button went dead after the first use');
});

test('a second click while the hold is up does not re-ask', async () => {
  const { stopper, calls } = harness();

  stopper.stop();
  await new Promise((r) => setImmediate(r));
  stopper.stop();
  await new Promise((r) => setImmediate(r));

  assert.deepEqual(calls, ['POST /admin/api/engine/7/interrupt']);
});

test('the wedge-breaker releases a hold nothing ever landed, and resyncs', async () => {
  const { stopper, chatInput, calls, timers } = harness();

  stopper.stop();
  await new Promise((r) => setImmediate(r));
  assert.equal(timers.length, 1, 'the hold must arm a backstop');

  timers[0]();
  await new Promise((r) => setImmediate(r));

  assert.equal(chatInput.isStopping(), false,
    'a held button is disabled AND suppresses Enter, so it can never wedge');
  assert.deepEqual(calls, [
    'POST /admin/api/engine/7/interrupt',
    'GET /admin/api/projects/7',
  ]);
});

test('the wedge-breaker is a no-op once the stop has landed', async () => {
  const { stopper, chatInput, calls, timers } = harness();

  stopper.stop();
  await new Promise((r) => setImmediate(r));
  chatInput.setStopping(false); // the landing arrived first

  timers[0]();
  await new Promise((r) => setImmediate(r));

  assert.deepEqual(calls, ['POST /admin/api/engine/7/interrupt'],
    'a stale backstop must not fire a stray status read at a view that moved on');
});

test('an in-flight chat turn wins: that is what the owner started last', async () => {
  const { stopper, sender, calls } = harness();
  sender.sending = true;

  stopper.stop();
  await new Promise((r) => setImmediate(r));

  assert.equal(sender.aborted, true);
  assert.deepEqual(calls, [], 'aborting the chat request must not also interrupt the pipeline');
});

test('seed marks the button working only for a build in flight', async () => {
  const building = harness({ phase: { key: 'building', stalled: false } });
  await building.stopper.seed();
  assert.equal(building.chatInput.working.get('engine'), true);

  for (const phase of [
    { key: 'live', stalled: false },
    { key: 'paused', stalled: false },
    { key: 'needs_you', stalled: false },
    { key: 'building', stalled: true },
  ]) {
    const idle = harness({ phase });
    await idle.stopper.seed();
    assert.equal(idle.chatInput.working.get('engine'), false, `${phase.key} stalled=${phase.stalled} is not in flight`);
  }
});
