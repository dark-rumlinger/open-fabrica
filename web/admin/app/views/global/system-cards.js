/**
 * Operator cards for System Settings: what the server is doing right now (version,
 * uptime, the messaging bot, database and queue pressure) and the local firewall
 * rule. Both are read from endpoints that already exist; nothing here changes
 * configuration except the firewall button, which asks the operating system for
 * permission itself.
 */

import { h, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { get, post } from '../../core/http.js';
import * as toast from '../../ui/toast.js';
import * as modal from '../../ui/modal.js';
import { humanizeError } from '../../ui/errors.js';
import { runAction } from '../../ui/actions.js';

const RELEASES_URL = 'https://github.com/dark-rumlinger/open-fabrica/releases';

/** "12 s ago", "3 min ago", "2 h ago" for a unix-seconds timestamp; '' when unknown. */
export function agoText(unixSeconds, nowMs = Date.now()) {
  if (!unixSeconds) return '';
  const s = Math.max(0, Math.round(nowMs / 1000 - unixSeconds));
  if (s < 90) return t('ago.seconds', { n: s });
  if (s < 90 * 60) return t('ago.minutes', { n: Math.round(s / 60) });
  return t('ago.hours', { n: Math.round(s / 3600) });
}

/**
 * The messaging bot in one line. `telegram` is /telegram/status (null when it
 * could not be read); `bot` is the process-wide counters from /system/status.
 */
export function botSummary(telegram, bot, nowMs = Date.now()) {
  if (!telegram) return t('bot.unknown');
  if (!telegram.configured) return t('bot.notSetUp');
  if (!telegram.running) return t('bot.notRunning');
  const seen = agoText(bot && bot.last_poll_unix, nowMs);
  if (bot && bot.healthy === false) {
    return bot.health_detail ? t('bot.problemDetail', { detail: bot.health_detail }) : t('bot.problem');
  }
  if (bot && bot.backoff_active) return seen ? t('bot.retryingSeen', { seen }) : t('bot.retrying');
  return seen ? t('bot.healthySeen', { seen }) : t('bot.healthy');
}

/**
 * Database and queue pressure in words an operator can act on. Busy retries are
 * normal in small numbers; "gave up" means a request failed because the database
 * stayed locked.
 */
export function pressureSummary(status) {
  const retries = status.sqlite_busy_retries || 0;
  const gaveUp = status.sqlite_busy_exhausted || 0;
  const dropped = status.background_commands_dropped || 0;
  const lines = [];
  lines.push(gaveUp > 0
    ? t('pressure.dbGaveUp', { gaveUp, retries })
    : t('pressure.dbOk', { retries }));
  lines.push(dropped > 0
    ? t('pressure.dropped', { dropped })
    : t('pressure.noneDropped'));
  return lines;
}

function row(label, value) {
  return h('tr', {}, [
    h('td', { className: 'text-secondary', style: { whiteSpace: 'nowrap', paddingInlineEnd: '16px', verticalAlign: 'top' } }, label),
    h('td', { style: { wordBreak: 'break-word' } }, value),
  ]);
}

/** What the server is doing right now, with a Refresh button. */
export function systemStatusCard() {
  const body = h('div', {}, [h('p', { className: 'form-hint' }, t('common.loading'))]);

  async function load() {
    try {
      const [status, telegram] = await Promise.all([
        get('/admin/api/system/status'),
        get('/admin/api/telegram/status').catch(() => null),
      ]);
      clear(body);
      const pressure = pressureSummary(status);
      body.appendChild(h('table', { className: 'table text-sm' }, [h('tbody', {}, [
        row(t('status.version'), [
          `${status.version || '-'} (${status.goos}/${status.goarch}, ${status.go_version}) `,
          h('a', { href: RELEASES_URL, target: '_blank', rel: 'noopener' }, t('status.checkRelease')),
        ]),
        row(t('status.runningFor'), status.uptime),
        row(t('nav.projects'), t('status.projectsValue', { count: status.project_count, running: status.brain_running })),
        row(t('status.bot'), botSummary(telegram, status.bot)),
        row(t('status.pressure'), h('div', {}, pressure.map((l) => h('div', {}, l)))),
        row(t('status.workers'), String(status.num_goroutines)),
      ])]));
    } catch (err) {
      clear(body);
      body.appendChild(h('p', { className: 'form-hint' }, t('status.loadFailed', { error: humanizeError(err.message) })));
    }
  }
  load();

  return h('div', { className: 'card mt-4' }, [
    h('div', { className: 'flex justify-between items-center mb-2' }, [
      h('h4', { className: 'card__title' }, t('status.title')),
      h('button', { className: 'btn btn--secondary btn--sm', onClick: load }, t('common.refresh')),
    ]),
    body,
  ]);
}

/**
 * The firewall rule Open Fabrica adds so other devices can reach the public site.
 * Shows whether it is there and offers the opposite action; both go through the
 * operating system's own permission prompt, which can be cancelled.
 */
export function firewallCard() {
  const body = h('div', {}, [h('p', { className: 'form-hint' }, t('common.loading'))]);

  async function load() {
    let status;
    try {
      status = await get('/admin/api/firewall/status');
    } catch (err) {
      clear(body);
      body.appendChild(h('p', { className: 'form-hint' }, t('firewall.readFailed', { error: humanizeError(err.message) })));
      return;
    }
    clear(body);

    if (!status.supported) {
      body.appendChild(h('p', { className: 'form-hint' }, status.detail || t('firewall.unsupported')));
      return;
    }
    if (!status.active) {
      body.appendChild(h('p', { className: 'form-hint' }, status.detail || t('firewall.inactive')));
      return;
    }

    if (status.rule_present) {
      body.appendChild(h('p', { className: 'form-hint' }, t('firewall.rulePresent')));
      body.appendChild(h('button', { className: 'btn btn--danger btn--sm mt-3', onClick: confirmRemove }, t('firewall.removeBtn')));
    } else {
      body.appendChild(h('p', { className: 'form-hint' },
        (status.detail ? status.detail + ' ' : '') + t('firewall.ruleMissing')));
      body.appendChild(h('button', { className: 'btn btn--primary btn--sm mt-3', onClick: () => change('allow') }, t('firewall.allowBtn')));
    }
  }

  function confirmRemove() {
    modal.show(
      t('firewall.removeTitle'),
      h('p', { className: 'text-secondary' }, t('firewall.removeBody')),
      [
        modal.cancelBtn(),
        { label: t('firewall.removeRule'), className: 'btn btn--danger', onClick: () => change('remove') },
      ],
    );
  }

  // The operating system answers with ok, cancelled (the prompt was dismissed) or an
  // error; each gets its own message, and the card re-reads the real state after.
  function change(action) {
    return runAction(async () => {
      const res = await post(`/admin/api/firewall/${action}`);
      if (res.cancelled) {
        toast.warning(t('firewall.cancelled'));
        return false;
      }
      if (!res.ok) {
        toast.error(res.error || t('firewall.failed'));
        return false;
      }
      toast.success(action === 'remove' ? t('firewall.removed') : t('firewall.allowed'));
      await load();
    }, { errorPrefix: t('firewall.changeFailed') });
  }

  load();

  return h('div', { className: 'card mt-4' }, [
    h('h4', { className: 'card__title mb-2' }, t('firewall.title')),
    body,
  ]);
}
