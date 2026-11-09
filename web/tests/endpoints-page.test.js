/**
 * Tests for where each read-only endpoint kind lives on the public site, as the APIs
 * tab shows it. A wrong URL there is a copy button that hands the owner a path that
 * 404s, so each kind's shape is pinned.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`).
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { endpointUrl } from '../admin/app/views/project/endpoints.js';

test('endpointUrl gives each kind its real public path', () => {
  assert.equal(endpointUrl({ kind: 'function', path: 'rates' }), '/api/rates');
  assert.equal(endpointUrl({ kind: 'agent', path: 'helper' }), '/api/helper');
  assert.equal(endpointUrl({ kind: 'document', path: 'invoice' }), '/api/invoice');
  assert.equal(endpointUrl({ kind: 'query', path: '_query' }), '/api/_query');
  assert.equal(endpointUrl({ kind: 'feed', path: 'news' }), '/feed/news');
  assert.equal(endpointUrl({ kind: 'export', path: 'orders' }), '/api/orders/export');
  assert.equal(endpointUrl({ kind: 'import', path: 'orders' }), '/api/orders/import');
  assert.equal(endpointUrl({ kind: 'push', path: 'alerts' }), '/api/alerts/push/subscribe');
});

test('two-factor and email-link endpoints are routes on their login API, not a path of their own', () => {
  assert.equal(endpointUrl({ kind: 'totp', path: 'auth' }), '/api/auth/totp/...');
  assert.equal(endpointUrl({ kind: 'magic_link', path: 'auth' }), '/api/auth/magic/...');
});

test('an unknown kind falls back to a plain /api/ path', () => {
  assert.equal(endpointUrl({ kind: 'something_new', path: 'x' }), '/api/x');
  assert.equal(endpointUrl({ path: 'x' }), '/api/x');
});
