/**
 * Project payments panel (owner only): what the project has taken, the plans it
 * sells and who holds a subscription. Read only, because these rows are written by
 * the payment provider's webhooks and the platform's checkout flow, never by hand.
 */

import { h, clear } from '../../core/dom.js';
import { t, tn, intlTag } from '../../core/i18n.js';
import { get } from '../../core/http.js';
import { emptyState, listSkeleton, showLoadError } from '../../ui/helpers.js';
import { pageHeader } from '../../ui/card.js';
import { formatDateTime } from '../../core/datetime.js';

/**
 * An amount held in minor units (cents) as money. The number of minor-unit digits
 * is the currency's own: 2 for euro and dollar, 0 for yen, 3 for dinar. An
 * unrecognised currency code falls back to the plain number and the code.
 */
export function formatMoney(minorUnits, currency) {
  const code = String(currency || '').toUpperCase();
  try {
    const fmt = new Intl.NumberFormat(intlTag(), { style: 'currency', currency: code });
    const digits = fmt.resolvedOptions().maximumFractionDigits;
    return fmt.format(minorUnits / 10 ** digits);
  } catch {
    return `${minorUnits} ${code}`.trim();
  }
}

/**
 * Total taken per currency, from the status/currency groups the server returns:
 * only payments that actually completed count as money.
 */
export function paidTotals(totals) {
  const byCurrency = {};
  for (const row of totals || []) {
    if (row.status !== 'paid') continue;
    const cur = String(row.currency || '').toLowerCase();
    byCurrency[cur] = byCurrency[cur] || { amount: 0, count: 0 };
    byCurrency[cur].amount += row.amount;
    byCurrency[cur].count += row.count;
  }
  return byCurrency;
}

const STATUS_BADGE = {
  paid: 'badge--success', active: 'badge--success', trialing: 'badge--info',
  pending: 'badge--warning', past_due: 'badge--warning', incomplete: 'badge--warning',
  failed: 'badge--danger', canceled: 'badge--danger', cancelled: 'badge--danger', expired: 'badge--danger',
};

// "past_due" -> "Past due": the stored status codes are not for reading. Known
// codes come from the catalog; an unknown one is spaced and capitalised.
function statusLabel(status) {
  if (!status) return '-';
  if (status in STATUS_BADGE) return t('pay.status.' + status);
  const words = String(status).replace(/_/g, ' ');
  return words.charAt(0).toUpperCase() + words.slice(1);
}

function badge(status) {
  return h('span', { className: `badge ${STATUS_BADGE[status] || ''}` }, statusLabel(status));
}

function table(headers, rows) {
  return h('div', { style: { overflowX: 'auto' } }, [
    h('table', { className: 'table text-sm' }, [
      h('thead', {}, [h('tr', {}, headers.map((label) => h('th', {}, label)))]),
      h('tbody', {}, rows.map((cells) => h('tr', {}, cells.map((c) => h('td', {}, c))))),
    ]),
  ]);
}

function section(title, hint, content) {
  return h('div', { className: 'card mt-4' }, [
    h('h4', { className: 'card__title mb-2' }, title),
    ...(hint ? [h('p', { className: 'form-hint' }, hint)] : []),
    content,
  ]);
}

export async function renderProjectPayments(container, projectId) {
  clear(container);
  const body = h('div', { className: 'context-panel__body' });
  container.appendChild(pageHeader({ title: t('tabs.payments') }));
  container.appendChild(body);

  let data;
  try {
    body.appendChild(listSkeleton(2));
    data = await get(`/admin/api/projects/${projectId}/payments`);
  } catch (err) {
    showLoadError(body, t('pay.loadFailed'), err, () => renderProjectPayments(container, projectId));
    return;
  }
  clear(body);

  const nothing = !data.configured && !data.payments.length && !data.plans.length && !data.subscriptions.length;
  if (nothing) {
    body.appendChild(emptyState({
      iconName: 'credit-card',
      title: t('pay.empty.title'),
      description: t('pay.empty.description'),
    }));
    return;
  }

  // Money taken, per currency; the count of payments still waiting is its own tile.
  const paid = paidTotals(data.totals);
  const pending = (data.totals || []).filter((row) => row.status === 'pending').reduce((n, row) => n + row.count, 0);
  const tiles = Object.entries(paid).map(([cur, v]) => h('div', { className: 'stat-card' }, [
    h('div', { className: 'stat-card__label' }, t('pay.taken', { currency: cur.toUpperCase() })),
    h('div', { className: 'stat-card__value' }, formatMoney(v.amount, cur)),
    h('div', { className: 'stat-card__sub' }, tn('pay.paidCount', v.count)),
  ]));
  if (!tiles.length) {
    tiles.push(h('div', { className: 'stat-card' }, [
      h('div', { className: 'stat-card__label' }, t('pay.takenPlain')),
      h('div', { className: 'stat-card__value' }, formatMoney(0, data.currency || 'usd')),
      h('div', { className: 'stat-card__sub' }, t('pay.noPaid')),
    ]));
  }
  tiles.push(h('div', { className: 'stat-card' }, [
    h('div', { className: 'stat-card__label' }, t('pay.waiting')),
    h('div', { className: 'stat-card__value' }, String(pending)),
    h('div', { className: 'stat-card__sub' }, t('pay.waitingSub')),
  ]));
  body.appendChild(h('div', { className: 'stat-cards' }, tiles));

  const providerLine = data.currency
    ? t('pay.providerWithCurrency', { provider: data.provider_type || t('pay.notSet'), currency: data.currency.toUpperCase() })
    : t('pay.provider', { provider: data.provider_type || t('pay.notSet') });
  body.appendChild(h('p', { className: 'form-hint mt-2' }, providerLine + ' ' + t('pay.stripeOnly')));

  body.appendChild(section(t('tabs.payments'), data.payments.length >= 200 ? t('pay.recent200') : '',
    data.payments.length
      ? table([t('pay.col.when'), t('pay.col.amount'), t('sp.status'), t('pay.col.what'), t('pay.col.ref')],
        data.payments.map((p) => [
          formatDateTime(p.created_at), formatMoney(p.amount, p.currency), badge(p.status),
          p.description || '', h('code', {}, p.external_id || ''),
        ]))
      : h('p', { className: 'form-hint' }, t('pay.noPayments'))));

  body.appendChild(section(t('pay.plans'), '',
    data.plans.length
      ? table([t('pay.col.plan'), t('pay.col.price'), t('pay.col.billed'), t('pay.col.trial'), t('pay.col.onSale')],
        data.plans.map((p) => [
          p.name, formatMoney(p.amount, p.currency), t('pay.every', { interval: p.interval }),
          p.trial_days ? t('pay.trialDays', { n: p.trial_days }) : t('pay.none'), p.is_active ? t('pay.yes') : t('pay.no'),
        ]))
      : h('p', { className: 'form-hint' }, t('pay.noPlans'))));

  body.appendChild(section(t('pay.subscriptions'), data.subscriptions.length >= 200 ? t('pay.recent200') : '',
    data.subscriptions.length
      ? table([t('pay.col.since'), t('pay.col.account'), t('pay.col.plan'), t('sp.status'), t('pay.col.renews'), t('pay.col.ref')],
        data.subscriptions.map((s) => [
          formatDateTime(s.created_at),
          s.user_id == null ? '-' : `#${s.user_id}`,
          s.plan || '-',
          badge(s.status),
          s.canceled_at ? t('pay.ended', { when: formatDateTime(s.canceled_at) }) : (s.current_period_end ? formatDateTime(s.current_period_end) : '-'),
          h('code', {}, s.external_id || ''),
        ]))
      : h('p', { className: 'form-hint' }, t('pay.noSubs'))));
}
