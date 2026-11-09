/**
 * Tests for the user flow test card wording (write_flows / test_flows results
 * and the go-live flow_check block). Run with `node --test "web/tests/*.test.js"`
 * (or `make test-js`).
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { flowsView, goLiveFlowView } from '../admin/app/ui/chat/cards/flows-view-model.js';

const failure = (name, step, of) =>
  `user flow "${name}" (of_test.js) failed at step ${step} of ${of} {"click":"#x"}: nothing matched\nJS errors while this step ran:\n  - boom`;

test('running titles say what the call does', () => {
  assert.equal(flowsView('write_flows', { source: 'x' }, null, 'running').title, 'Saving and replaying flows');
  assert.equal(flowsView('test_flows', {}, null, 'running').title, 'Replaying all flows');
  assert.equal(flowsView('test_flows', { flow: 'checkout' }, null, 'running').title, 'Replaying "checkout"');
  assert.equal(flowsView('test_flows', {}, null, 'running').state, 'running');
});

test('a passing replay reads as passed, and a save says it was also replayed', () => {
  const res = { success: true, data: { passed: ['a', 'b', 'c'], failed: null } };
  const v = flowsView('test_flows', {}, res, 'success');
  assert.equal(v.state, 'passed');
  assert.equal(v.title, 'All 3 flows passed');
  assert.equal(v.expanded, false);

  const saved = { success: true, data: { passed: ['a', 'b'], failed: null, flows: ['a', 'b'], steps: 12, saved: 'of_test.js' } };
  const w = flowsView('write_flows', {}, saved, 'success');
  assert.equal(w.sub, 'Saved and replayed.');
  assert.deepEqual(w.saved, { flows: 2, steps: 12 });
});

test('a saved script with a red replay is failed, not rejected', () => {
  const res = {
    success: false,
    error: '1 of 2 flows failed: b',
    data: { passed: ['a'], failed: ['b'], flows: ['a', 'b'], steps: 9, saved: 'of_test.js', failures: [failure('b', 3, 5)] },
  };
  const v = flowsView('write_flows', {}, res, 'success');
  assert.equal(v.state, 'failed');
  assert.equal(v.title, '1 of 2 flows failed');
  assert.equal(v.sub, 'Saved. Failed: b');
  assert.deepEqual(v.rows.map((r) => [r.name, r.status]), [['a', 'passed'], ['b', 'failed']]);
  assert.equal(v.rows[1].detail, 'Step 3 of 5');
  // The row gives the human line, not the raw step.
  assert.match(v.rows[1].reason, /^nothing matched/);
  assert.doesNotMatch(v.rows[1].reason, /click/);
  assert.ok(v.expanded);
});

test('failure text alone is enough for a red summary', () => {
  const v = flowsView('test_flows', {}, { success: false, error: '2 of 4 flows failed: a, b (1 more blocked by them)' }, 'error');
  assert.equal(v.state, 'failed');
  assert.equal(v.title, '2 of 4 flows failed');
  assert.equal(v.sub, 'Failed: a, b (1 waiting on them)');
  assert.equal(flowsView('test_flows', {}, '2 of 4 flows failed: a, b', 'error').state, 'failed');
});

// The step is printed the way the runner prints it: JSON with > style escapes.
const GT = '\\' + 'u003e';
const AMP = '\\' + 'u0026';
const REAL_STEP = `{"expectJs":"(() =${GT} { const b = document.querySelector('.msg.santa .bubble span'); return b ${AMP}${AMP} b.textContent.trim().length ${GT} 0; })()","timeout":20000}`;
const REAL_DETAIL = `expectJs "(() => { const b = document.querySelector('.msg.santa .bubble span'); return b && b.textContent.trim().length > 0; })()" stayed falsy (last value: false) (waited 20s) (page /)`;
const REAL_FAILURE = [
  `user flow "first-load welcome" (of_test.js) failed at step 7 of 8 ${REAL_STEP}: ${REAL_DETAIL}`,
  'Page at failure: / ("Bad Santa")',
  'Visible controls (usable selectors): #sendBtn "Send"',
  'Verdict: the page responded, but not as the step expected. If the app is wrong fix the app.',
].join('\n');

test('a long failure gives a short row reason and a report that adds to it', () => {
  const res = { success: false, error: '1 of 2 flows failed: first-load welcome', data: { passed: ['b'], failed: ['first-load welcome'], failures: [REAL_FAILURE] } };
  const row = flowsView('test_flows', {}, res, 'success').rows.find((r) => r.name === 'first-load welcome');
  assert.equal(row.detail, 'Step 7 of 8');
  // Short, no script dump, no repeated step.
  assert.ok(row.reason.length <= 160, row.reason);
  assert.match(row.reason, /^expectJs "\(\(\) => \{ const b = document\.quer.*\.\.\." stayed falsy \(last value: false\)/);
  // The report decodes the step and keeps the page state, but drops advice for the model.
  assert.ok(row.full.startsWith('Step\n{\n  "expectJs": "(() => {'), row.full);
  assert.match(row.full, /"timeout": 20000/);
  // The script appears once, in the step; the failure points at it instead of repeating it.
  assert.equal(row.full.split('querySelector').length - 1, 1, row.full);
  assert.match(row.full, /Failure\nexpectJs "\.\.\." stayed falsy \(last value: false\) \(waited 20s\) \(page \/\)/);
  assert.match(row.full, /Page at failure: \/ \("Bad Santa"\)/);
  assert.match(row.full, /Visible controls/);
  assert.doesNotMatch(row.full, /Verdict:/);
  assert.doesNotMatch(row.full, /u003e/);
});

test('a failure the row already says in full has no report to open', () => {
  const msg = 'user flow "a" (of_test.js) failed at step 2 of 3 {"click":"#go"}: nothing matched "#go"\nVerdict: fix the app.';
  const res = { success: false, error: '1 of 1 flows failed: a', data: { failed: ['a'], failures: [msg] } };
  const row = flowsView('test_flows', {}, res, 'success').rows[0];
  assert.equal(row.reason, 'nothing matched "#go"');
  assert.equal(row.full, '');
});

test('blocked flows are shown as waiting, not failed', () => {
  const res = { success: false, error: '1 of 3 flows failed: a', data: { passed: ['b'], failed: ['a'], blocked: ['c (needs {{link}})'] } };
  const v = flowsView('test_flows', {}, res, 'success');
  const c = v.rows.find((r) => r.name === 'c');
  assert.equal(c.status, 'blocked');
  assert.equal(c.detail, 'Waiting on a failed flow');
});

test('a skipped replay says why, and is never shown as passed', () => {
  const host = flowsView('test_flows', {}, { success: true, data: { skipped: 'headless browser unavailable', skip_kind: 'no_browser' } }, 'success');
  assert.equal(host.state, 'skipped');
  assert.equal(host.title, 'Not replayed on this host');
  const busy = flowsView('test_flows', {}, { success: true, data: { skipped: 'real visitors used the app', skip_kind: 'visitors' } }, 'success');
  assert.equal(busy.title, 'Not replayed: visitors are on the site');
  const unknown = flowsView('test_flows', {}, { success: true, data: { skipped: 'something new' } }, 'success');
  assert.equal(unknown.title, 'Not replayed');
  const saved = flowsView('write_flows', {}, { success: true, data: { saved: 'of_test.js', flows: ['a'], steps: 2, replay_skipped: 'unreachable' } }, 'success');
  assert.equal(saved.title, 'Saved, not replayed');
});

test('budget and stall states', () => {
  assert.equal(flowsView('test_flows', {}, { success: true, data: { budget_exhausted: true } }, 'success').title, 'Replay limit reached');
  assert.equal(flowsView('test_flows', {}, { success: true, data: { stalled: true } }, 'success').title, 'Replays paused');
});

test('a refusal that saved nothing is rejected, with its reason', () => {
  const dropped = flowsView('write_flows', {}, { success: false, code: 'flows_dropped', error: 'not saved: this script leaves out 1 saved flow(s)' }, 'error');
  assert.equal(dropped.state, 'rejected');
  assert.equal(dropped.title, 'Flows not saved');
  assert.match(dropped.sub, /delete saved flows/);
  const none = flowsView('test_flows', {}, { success: false, error: 'no of_test.js saved yet; write one first' }, 'error');
  assert.equal(none.title, 'Flows not replayed');
  assert.equal(none.sub, 'no of_test.js saved yet; write one first');
});

test('tests turned off', () => {
  const v = flowsView('write_flows', {}, { success: false, code: 'user_flows_off', error: 'turned off' }, 'error');
  assert.equal(v.state, 'off');
  assert.equal(v.title, 'User flow tests are off');
});

test('footnotes come from weak checks, rollback problems and warnings', () => {
  const res = { success: true, data: { passed: ['a'], weak_checks: ['x', 'y'], rollback_error: 'could not restore', warnings: ['w'], replays_left: 1 } };
  assert.deepEqual(flowsView('test_flows', {}, res, 'success').notes, [
    '2 steps passed but cannot prove much. Worth strengthening.',
    'could not restore',
    '1 warning in the script.',
    '1 replay left this hour.',
  ]);
});

test('anything unparseable or empty still yields a neutral card', () => {
  assert.equal(flowsView('test_flows', {}, '{"success":true,"data":{"passed":["a"', 'success').state, 'unknown');
  assert.equal(flowsView('test_flows', {}, null, 'success').state, 'unknown');
});

test('go-live block reports pass, fail and skip honestly', () => {
  const ok = goLiveFlowView({ total: 5, failed: [] });
  assert.equal(ok.title, 'All 5 flows passed');
  assert.equal(ok.tone, 'success');

  const red = goLiveFlowView({ total: 5, failed: ['login', 'cart'] });
  assert.equal(red.title, '2 of 5 flows did not pass');
  assert.deepEqual(red.failed, ['login', 'cart']);

  const legacy = goLiveFlowView({ failed: ['login'] });
  assert.equal(legacy.title, '1 flow did not pass');

  const skipped = goLiveFlowView({ total: 5, skipped: true, skip_kind: 'no_browser' });
  assert.equal(skipped.state, 'skipped');
  assert.equal(skipped.title, 'Not replayed on this host');

  assert.equal(goLiveFlowView(null), null);
  assert.equal(goLiveFlowView({ total: 0, failed: [] }), null);
});
