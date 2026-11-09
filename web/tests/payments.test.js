/**
 * Tests for the money handling behind the Payments panel. Amounts arrive in minor
 * units and every currency has its own number of them, so a mistake here shows up as
 * a price off by a factor of a hundred; and only completed payments may count as
 * money taken.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`).
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { formatMoney, paidTotals } from '../admin/app/views/project/payments.js';

// Compare digits only: the thousands separator and symbol placement are the
// machine's locale, which is exactly what the panel should follow.
const digits = (s) => s.replace(/[^0-9.,]/g, '');

test('formatMoney divides by the currency\'s own minor units', () => {
  assert.match(formatMoney(1999, 'eur'), /19[.,]99/);
  assert.match(formatMoney(1999, 'USD'), /19[.,]99/);
  assert.equal(digits(formatMoney(1999, 'jpy')).replace(/[.,]/g, ''), '1999'); // yen has no minor unit
  assert.match(formatMoney(1999, 'kwd'), /1[.,]999/); // dinar has three
  assert.match(formatMoney(0, 'eur'), /0[.,]00/);
});

test('formatMoney survives a currency code it does not know', () => {
  assert.equal(formatMoney(500, 'not-a-currency'), '500 NOT-A-CURRENCY');
  assert.equal(formatMoney(500, ''), '500');
});

test('paidTotals counts only completed payments, per currency', () => {
  const totals = [
    { status: 'paid', currency: 'eur', count: 2, amount: 4499 },
    { status: 'pending', currency: 'eur', count: 5, amount: 9000 },
    { status: 'failed', currency: 'eur', count: 1, amount: 100 },
    { status: 'paid', currency: 'USD', count: 1, amount: 300 },
    { status: 'paid', currency: 'usd', count: 1, amount: 200 },
  ];
  assert.deepEqual(paidTotals(totals), {
    eur: { amount: 4499, count: 2 },
    usd: { amount: 500, count: 2 }, // upper and lower case are one currency
  });
  assert.deepEqual(paidTotals([]), {});
  assert.deepEqual(paidTotals(undefined), {});
});
