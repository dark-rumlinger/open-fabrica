/**
 * Tests for ui/errors.js humanizeError.
 *
 * The status-word patterns used to be bare /401|403|404/ and /not found/, which
 * rewrote specific server and provider messages ("Table 'orders' not found",
 * "provider returned 404 for model x") into a generic line. These pin that a
 * status word only maps when it is the whole message.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`). See markdown.test.js for
 * why these live outside web/admin.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

const { humanizeError } = await import('../admin/app/ui/errors.js');

test('humanizeError: a bare status word maps to plain language', () => {
  assert.match(humanizeError('Unauthorized'), /session expired/i);
  assert.match(humanizeError('Forbidden'), /permission/i);
  assert.match(humanizeError('Not Found'), /couldn't find/i);
});

test('humanizeError: the http.js fallback message maps by status', () => {
  assert.match(humanizeError('Request failed (401)'), /session expired/i);
  assert.match(humanizeError('Request failed (403)'), /permission/i);
  assert.match(humanizeError('Request failed (404)'), /couldn't find/i);
});

test('humanizeError: the fallback for any other status keeps its number', () => {
  assert.match(humanizeError('Request failed (500)'), /could not complete the request \(error 500\)/i);
  assert.match(humanizeError('Request failed (429) '), /error 429/);
  assert.equal(humanizeError('Request failed (5000)'), 'Request failed (5000)');
});

test('humanizeError: specific messages that merely contain a status are kept', () => {
  const msgs = [
    "Table 'orders' not found",
    'provider returned 404 for model gpt-x',
    'listening on port 4010',
    'unauthorized: key revoked by provider',
  ];
  for (const m of msgs) assert.equal(humanizeError(m), m);
});

test('humanizeError: network failures still map, including Safari wording', () => {
  assert.match(humanizeError('Failed to fetch'), /network problem/i);
  assert.match(humanizeError('Load failed'), /network problem/i);
});

test('humanizeError: Error objects and empty values are handled', () => {
  assert.match(humanizeError(new Error('Forbidden')), /permission/i);
  assert.equal(humanizeError(''), '');
  assert.equal(humanizeError(undefined), undefined);
});
