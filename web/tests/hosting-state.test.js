/**
 * Tests for the hosting-state classifier, the single home for "is this project
 * reachable, and what do we call that". Both the status chip and the Visitors
 * strip render from it, so a mistake here shows up in two places at once.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`). No framework and no
 * dependencies: node:test and node:assert are built in.
 *
 * What these pin: a failed self-probe is not proof that a site is broken. The server
 * dials its own public hostname from the machine that serves it, and most home routers
 * refuse to loop that back, so the dial fails on a perfectly healthy site. Whether a
 * state is a success or a failure must be decided in exactly one place, and these tests
 * hold it there.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { classifyHosting } from '../admin/app/ui/hosting-state.js';

const URL = 'https://mysite.com';

/** A public-domain status payload with the HTTPS slice overridden per test. */
function publicStatus(https) {
  return { domain: 'mysite.com', domain_kind: 'public', tls_disabled: false, https };
}

test('a working site is live', () => {
  const view = classifyHosting(publicStatus({ state: 'active', cert_loaded: true }), { url: URL });
  assert.equal(view.key, 'live');
  assert.equal(view.kind, 'success');
  assert.ok(!view.needsPhoneCheck);
});

test('unverified is a success, not a problem', () => {
  const view = classifyHosting(publicStatus({ state: 'unverified', cert_loaded: true }), { url: URL });
  assert.equal(view.key, 'unverified');
  assert.equal(view.kind, 'success');
  assert.ok(view.needsPhoneCheck, 'only a device off this network can settle it');
});

test('a failed probe with a loaded certificate is not a failure', () => {
  // The regression: issuance worked and 443 is served, so the failed dial is the
  // router refusing to hairpin. Older recorded statuses still look like this.
  const view = classifyHosting(
    publicStatus({ state: 'failed', cert_loaded: true, hint: 'connection timed out' }),
    { url: URL }
  );
  assert.equal(view.key, 'unverified');
  assert.notEqual(view.kind, 'danger');
  assert.ok(view.needsPhoneCheck);
});

test('a failed probe with no certificate is a real problem, and carries the hint', () => {
  const view = classifyHosting(
    publicStatus({ state: 'failed', cert_loaded: false, hint: 'DNS is not pointing here' }),
    { url: URL }
  );
  assert.equal(view.key, 'problem');
  assert.equal(view.kind, 'danger');
  assert.match(view.sentence, /DNS is not pointing here/);
});

test('an unprobed domain is pending, and keeps polling', () => {
  const view = classifyHosting(publicStatus({ state: 'pending', cert_loaded: false }), { url: URL });
  assert.equal(view.key, 'pending');
  assert.notEqual(view.kind, 'danger');
  assert.ok(view.pending);
});

test('domain kind outranks the HTTPS slice', () => {
  // hosting.go reports not_applicable for these, but the kind alone decides:
  // no amount of HTTPS state makes a localhost project internet-reachable.
  const local = classifyHosting(
    { domain: 'app.localhost', domain_kind: 'local', https: { state: 'not_applicable' } },
    { url: 'http://app.localhost:5733' }
  );
  assert.equal(local.key, 'local');

  const lan = classifyHosting(
    { domain: '192.168.1.10', domain_kind: 'ip', https: { state: 'not_applicable' } },
    { url: 'http://192.168.1.10:5733' }
  );
  assert.equal(lan.key, 'lan');

  const none = classifyHosting({ domain: '', domain_kind: 'none', https: {} }, {});
  assert.equal(none.key, 'no-domain');
});

test('TLS switched off for the instance is live, not broken', () => {
  const view = classifyHosting(
    { domain: 'mysite.com', domain_kind: 'public', tls_disabled: true, https: { state: 'disabled' } },
    { url: 'http://mysite.com' }
  );
  assert.equal(view.key, 'tls-off');
  assert.notEqual(view.kind, 'danger');
});

test('every state names itself for both surfaces', () => {
  // The chip and the strip word the same state differently on purpose; neither
  // may come back empty, or a badge renders blank.
  const payloads = [
    publicStatus({ state: 'active' }),
    publicStatus({ state: 'unverified', cert_loaded: true }),
    publicStatus({ state: 'failed', cert_loaded: false }),
    publicStatus({ state: 'pending' }),
    { domain: '', domain_kind: 'none', https: {} },
    { domain: 'a.localhost', domain_kind: 'local', https: {} },
    { domain: '10.0.0.4', domain_kind: 'ip', https: {} },
    { domain: 'mysite.com', domain_kind: 'public', tls_disabled: true, https: { state: 'disabled' } },
  ];
  for (const st of payloads) {
    const view = classifyHosting(st, { url: URL });
    assert.ok(view.key, 'key is set');
    assert.ok(view.chip, `chip label for ${view.key}`);
    assert.ok(view.label, `strip label for ${view.key}`);
    assert.ok(view.sentence, `sentence for ${view.key}`);
    assert.ok(view.kind, `badge kind for ${view.key}`);
  }
});

test('no hosting label uses the word live, which names the project phase', () => {
  const statuses = [
    { domain_kind: 'none' },
    { domain_kind: 'local' },
    { domain_kind: 'ip' },
    publicStatus({ state: 'active', cert_loaded: true }),
    publicStatus({ state: 'unverified', cert_loaded: true }),
    publicStatus({ state: 'failed', cert_loaded: true }),
    publicStatus({ state: 'failed', cert_loaded: false }),
    publicStatus({ state: 'pending' }),
    { ...publicStatus({ state: 'disabled' }), tls_disabled: true },
  ];
  for (const st of statuses) {
    const view = classifyHosting(st, { url: URL });
    assert.doesNotMatch(view.label, /live/i, `label "${view.label}" for ${JSON.stringify(st)}`);
    assert.doesNotMatch(view.chip, /live/i, `chip "${view.chip}" for ${JSON.stringify(st)}`);
  }
});
