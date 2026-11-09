/**
 * Tests for the phase view, the single home for "what phase is this project in,
 * and what do we call it". The project header, the projects list, the project
 * switcher, the chat stat card and the pipeline feed all render from it.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`).
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { describePhase, phaseAction } from '../admin/app/ui/phase-state.js';

const phase = (key, extra = {}) => ({ key, label: '', reason: '', pending: 0, stalled: false, ...extra });

test('the server reason is the sentence when there is one', () => {
  const view = describePhase(phase('needs_you', { reason: '2 questions need your answer' }));
  assert.equal(view.sentence, '2 questions need your answer');
  assert.equal(view.reason, '2 questions need your answer');
});

test('without a reason the sentence explains the phase', () => {
  const view = describePhase(phase('building'));
  assert.equal(view.reason, '');
  assert.match(view.sentence, /building/i);
});

test('labels name the four phases', () => {
  assert.equal(describePhase(phase('building')).label, 'Building');
  assert.equal(describePhase(phase('needs_you')).label, 'Needs you');
  assert.equal(describePhase(phase('live')).label, 'Live');
  assert.equal(describePhase(phase('paused')).label, 'Paused');
});

test('only live is a success, and needing the owner is a warning', () => {
  assert.equal(describePhase(phase('live')).kind, 'success');
  assert.equal(describePhase(phase('needs_you')).kind, 'warning');
  assert.notEqual(describePhase(phase('paused')).kind, 'success');
  assert.notEqual(describePhase(phase('building')).kind, 'success');
});

test('a building project with a shut down engine is a warning', () => {
  const view = describePhase(phase('building', { stalled: true, reason: 'The engine is shut down' }));
  assert.equal(view.kind, 'warning');
  assert.equal(view.dot, 'status-dot--warning');
  assert.equal(view.sentence, 'The engine is shut down');
});

test('a pause after an error wears warning colours, not the quiet grey of a deliberate pause', () => {
  const stopped = describePhase(phase('paused', { reason: 'Stopped after an error', reason_key: 'stopped_error' }));
  assert.equal(stopped.error, true);
  assert.equal(stopped.kind, 'warning');
  assert.equal(stopped.dot, 'status-dot--warning');
  // A deliberate pause stays plain grey and is not flagged as an error.
  const byOwner = describePhase(phase('paused', { reason: 'Paused by you', reason_key: 'paused_owner' }));
  assert.equal(byOwner.error, false);
  assert.equal(byOwner.kind, 'secondary');
  assert.equal(byOwner.dot, '');
});

test('each phase has its own dot, and paused is the plain one', () => {
  assert.equal(describePhase(phase('building')).dot, 'status-dot--active');
  assert.equal(describePhase(phase('needs_you')).dot, 'status-dot--warning');
  assert.equal(describePhase(phase('live')).dot, 'status-dot--live');
  assert.equal(describePhase(phase('paused')).dot, '');
});

test('each phase offers one action', () => {
  assert.deepEqual(phaseAction(phase('needs_you')), { id: 'answer', label: 'Answer questions' });
  assert.deepEqual(phaseAction(phase('live')), { id: 'pause', label: 'Pause', interrupt: false });
  assert.deepEqual(phaseAction(phase('building')), { id: 'pause', label: 'Pause', interrupt: true });
});

test('a shut down engine offers to start it instead of pausing', () => {
  assert.equal(phaseAction(phase('building', { stalled: true })).id, 'start');
});

test('resuming returns to where the server says the project was', () => {
  assert.equal(phaseAction(phase('paused', { resume_to: 'live' })).to, 'live');
  assert.equal(phaseAction(phase('paused', { resume_to: 'building' })).to, 'building');
});

test('the word live is only ever a phase label', () => {
  for (const key of ['building', 'needs_you', 'paused']) {
    assert.doesNotMatch(describePhase(phase(key)).label, /live/i);
  }
});

test('a reason key is said in the interface language, with the count for questions', () => {
  const said = (key, extra) => describePhase(phase('needs_you', { reason: 'server english', reason_key: key, ...extra })).reason;
  assert.equal(said('answers', { pending: 1 }), '1 question needs your answer');
  assert.equal(said('answers', { pending: 3 }), '3 questions need your answer');
  assert.equal(said('answers_none'), 'Waiting for your answer');
  assert.equal(said('approval'), 'Waiting for your approval');
  assert.equal(said('engine_down'), 'The engine is shut down');
  assert.equal(said('stopped_owner'), 'Stopped by you');
  assert.equal(said('stopped_error'), 'Stopped after an error');
  assert.equal(said('paused_owner'), 'Paused by you');
});

test('a reason key this build does not know falls back to the server line', () => {
  const view = describePhase(phase('paused', { reason: 'Paused for maintenance', reason_key: 'maintenance' }));
  assert.equal(view.reason, 'Paused for maintenance');
  assert.equal(view.sentence, 'Paused for maintenance');
});

test('the UI handles every reason key the server can send', async () => {
  const { readFileSync } = await import('node:fs');
  const go = readFileSync(new URL('../../internal/projectphase/projectphase.go', import.meta.url), 'utf8');
  const keys = [...go.matchAll(/Reason\w+\s*=\s*"([a-z_]+)"/g)].map((m) => m[1]);
  assert.ok(keys.length >= 7, 'reason keys not found in projectphase.go');
  for (const key of keys) {
    const view = describePhase(phase('paused', { reason: 'SERVER LINE', reason_key: key, pending: 2 }));
    assert.notEqual(view.reason, 'SERVER LINE', `no translation for reason key ${key}`);
  }
});
