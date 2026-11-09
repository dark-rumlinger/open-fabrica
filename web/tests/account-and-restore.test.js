/**
 * Tests for the small pure pieces behind the Account dialog, the version-history
 * Restore button and the setup wizard's token header. The DOM parts of those
 * screens are covered by the owner's UI pass; what is pinned here is the logic a
 * typo would silently break: the password rules must match the server's, the
 * restore URL must be derived from the versions URL, and the setup token must ride
 * in the header only when one was entered.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`).
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { passwordProblem } from '../admin/app/ui/account-dialog.js';
import { restoreUrlFor } from '../admin/app/ui/resource-cards.js';
import { setupAuth } from '../admin/app/views/setup/setup-auth.js';

test('passwordProblem mirrors the server rules, one sentence per failure', () => {
  assert.equal(passwordProblem('Ab1!'), 'Password must be at least 8 characters');
  assert.equal(passwordProblem('abcdefg1!'), 'Password must contain an uppercase letter');
  assert.equal(passwordProblem('Abcdefgh!'), 'Password must contain a number');
  assert.equal(passwordProblem('Abcdefg12'), 'Password must contain a special character');
  assert.equal(passwordProblem('Abcdefg1!'), null);
  assert.equal(passwordProblem('correct-Horse-9-battery'), null);
});

test('restoreUrlFor swaps only the trailing /versions segment', () => {
  assert.equal(restoreUrlFor('/admin/api/projects/4/files/7/versions'), '/admin/api/projects/4/files/7/restore');
  assert.equal(restoreUrlFor('/admin/api/projects/4/pages/2/versions'), '/admin/api/projects/4/pages/2/restore');
  assert.equal(restoreUrlFor('/admin/api/projects/4/layouts/1/versions'), '/admin/api/projects/4/layouts/1/restore');
  assert.equal(restoreUrlFor('/admin/api/projects/4/components/9/versions'), '/admin/api/projects/4/components/9/restore');
  // A project or file that happens to be named "versions" earlier in the path stays.
  assert.equal(restoreUrlFor('/admin/api/projects/4/files/7/versions/versions'), '/admin/api/projects/4/files/7/versions/restore');
});

test('setupAuth adds the header only when a token was entered', () => {
  assert.equal(setupAuth({ setupToken: '' }), undefined);
  assert.equal(setupAuth({}), undefined);
  assert.equal(setupAuth(undefined), undefined);
  assert.deepEqual(setupAuth({ setupToken: 'abc123' }), { headers: { 'X-Setup-Token': 'abc123' } });
});
