/**
 * Tests for the wording behind the System Settings status card. The card turns raw
 * counters into sentences an operator can act on, so the thresholds and the
 * "what do I do" text are what is pinned here: a bot that is not set up must not
 * read as a failure, a database that gave up on a lock must say so, and a quiet
 * server must not cry wolf.
 *
 * Run with `node --test "web/tests/*.test.js"` (or `make test-js`).
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { agoText, botSummary, pressureSummary } from '../admin/app/views/global/system-cards.js';

const NOW = 1_800_000_000_000;
const sec = (n) => NOW / 1000 - n;

test('agoText speaks in the unit that fits', () => {
  assert.equal(agoText(0, NOW), '');
  assert.equal(agoText(sec(12), NOW), '12 s ago');
  assert.equal(agoText(sec(300), NOW), '5 min ago');
  assert.equal(agoText(sec(3 * 3600), NOW), '3 h ago');
  assert.equal(agoText(sec(-5), NOW), '0 s ago'); // a clock a little ahead never shows a negative
});

test('botSummary separates "not set up" from "broken"', () => {
  assert.equal(botSummary(null, {}, NOW), 'Unknown');
  assert.equal(botSummary({ configured: false }, {}, NOW), 'Not set up');
  assert.equal(botSummary({ configured: true, running: false }, {}, NOW), 'Set up, but not running');
  assert.equal(
    botSummary({ configured: true, running: true }, { healthy: true, last_poll_unix: sec(8) }, NOW),
    'Healthy, last reached Telegram 8 s ago',
  );
  assert.equal(
    botSummary({ configured: true, running: true }, { healthy: false, health_detail: 'the token was rejected' }, NOW),
    'Problem: the token was rejected',
  );
  assert.match(
    botSummary({ configured: true, running: true }, { healthy: true, backoff_active: true, last_poll_unix: sec(40) }, NOW),
    /^Retrying after an error, last reached Telegram 40 s ago$/,
  );
});

test('pressureSummary is quiet when quiet and specific when not', () => {
  const calm = pressureSummary({ sqlite_busy_retries: 3, sqlite_busy_exhausted: 0, background_commands_dropped: 0 });
  assert.equal(calm.length, 2);
  assert.match(calm[0], /no waits \(3 short retries/);
  assert.equal(calm[1], 'Background work: nothing turned away.');

  const loaded = pressureSummary({ sqlite_busy_retries: 90, sqlite_busy_exhausted: 2, background_commands_dropped: 5 });
  assert.match(loaded[0], /2 request\(s\) gave up waiting for a lock \(90 retries\)/);
  assert.match(loaded[0], /server logs/);
  assert.match(loaded[1], /5 command\(s\) were turned away/);
  assert.match(loaded[1], /AI provider/);

  // Missing counters (an older server) read as zero, not as NaN or "undefined".
  const bare = pressureSummary({});
  assert.doesNotMatch(bare.join(' '), /undefined|NaN/);
});
