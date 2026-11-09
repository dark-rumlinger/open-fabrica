/**
 * Tests for core/format.js in the default (English) interface.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`).
 */

import test from 'node:test';
import assert from 'node:assert/strict';

const { formatNumber, formatCompact, formatBytes, formatUsd, formatPrice, formatDuration, formatRatio } =
  await import('../admin/app/core/format.js');

test('numbers are grouped', () => {
  assert.equal(formatNumber(1234567), '1,234,567');
  assert.equal(formatNumber(0), '0');
  assert.equal(formatNumber(null), '0');
  assert.equal(formatNumber('12'), '12');
});

test('compact numbers keep the order of magnitude', () => {
  assert.equal(formatCompact(128000), '128K');
  assert.equal(formatCompact(1500000), '1.5M');
  assert.equal(formatCompact(512), '512');
});

test('byte sizes step through B, KB and MB', () => {
  assert.equal(formatBytes(0), '0 byte');
  assert.match(formatBytes(1536), /^1\.5 kB$/);
  assert.match(formatBytes(5 * 1024 * 1024), /^5 MB$/);
  assert.match(formatBytes(undefined), /^0 /);
});

test('dollars keep cents; prices drop empty ones', () => {
  assert.equal(formatUsd(3), '$3.00');
  assert.equal(formatUsd(0), '$0.00');
  assert.equal(formatPrice(3), '$3');
  assert.equal(formatPrice(0.15), '$0.15');
  assert.equal(formatPrice(0.5), '$0.5');
});

test('durations step through ms, seconds and minutes', () => {
  assert.equal(formatDuration(250), '250ms');
  assert.equal(formatDuration(1500), '1.5s');
  assert.equal(formatDuration(0), '0ms');
  assert.equal(formatDuration(60000), '1m 0s');
  assert.equal(formatDuration(90000), '1m 30s');
  assert.equal(formatDuration(3600000), '60m 0s');
});

test('ratios show one decimal and an x', () => {
  assert.equal(formatRatio(1.234), '1.2x');
  assert.equal(formatRatio(2), '2.0x');
  assert.equal(formatRatio(undefined), '0.0x');
});
