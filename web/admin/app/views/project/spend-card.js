/**
 * The "Daily AI limits" card in Settings > Advanced. Two token pools: unattended
 * model calls (the build loop, triggers, jobs, server functions) and the public AI
 * endpoints the app's visitors call (LLM chat, agents). The dollar cap covers both.
 * When a limit is reached that pool stops until midnight UTC. A blank field means
 * "use the default": the instance token budget, the public default, and no dollar cap.
 */

import { h, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { formatNumber, formatUsd } from '../../core/format.js';
import { get, put } from '../../core/http.js';
import * as toast from '../../ui/toast.js';
import { humanizeError } from '../../ui/errors.js';
import { formGroup } from '../../ui/helpers.js';

const fmtInt = (n) => formatNumber(n);
const fmtUsd = (n) => formatUsd(n);

/** Parses a blank-or-number input: '' -> null, a bad value -> undefined. */
function readNumber(input, { integer }) {
  const raw = input.value.trim();
  if (raw === '') return null;
  const n = Number(raw);
  if (!Number.isFinite(n) || n < 0 || (integer && !Number.isInteger(n))) return undefined;
  return n;
}

export function createSpendCard(projectId) {
  const body = h('div');
  const card = h('div', { className: 'card' }, [
    h('h4', { className: 'card__title mb-2' }, t('spend.title')),
    body,
  ]);

  function paint(s) {
    clear(body);
    const effective = s.effective_token_budget;
    const tokensInput = h('input', {
      className: 'input', type: 'number', min: '0', step: '1000',
      value: s.daily_token_budget ?? '',
      placeholder: t('spend.defaultPlaceholder', {
        value: effective === 0 ? t('settings.unlimited') : fmtInt(s.instance_token_budget),
      }),
    });
    const publicEffective = s.effective_public_token_budget;
    const publicInput = h('input', {
      className: 'input', type: 'number', min: '0', step: '1000',
      value: s.daily_public_token_budget ?? '',
      placeholder: t('spend.defaultPlaceholder', {
        value: publicEffective === 0 ? t('settings.unlimited') : fmtInt(s.default_public_token_budget),
      }),
    });
    const usdInput = h('input', {
      className: 'input', type: 'number', min: '0', step: '0.5',
      value: s.daily_usd_cap ?? '',
      placeholder: t('spend.noCap'),
    });

    const tokensLine = effective
      ? t('spend.todayOf', { used: fmtInt(s.tokens_used_today), limit: fmtInt(effective) })
      : t('spend.today', { used: fmtInt(s.tokens_used_today) });
    const usdLine = s.daily_usd_cap
      ? t('spend.usdOf', { used: fmtUsd(s.usd_used_today), cap: fmtUsd(s.daily_usd_cap) })
      : t('spend.usdSpent', { used: fmtUsd(s.usd_used_today) });
    body.appendChild(h('p', { className: 'form-hint' },
      t('spend.intro', { usage: `${tokensLine} - ${usdLine}` })));
    body.appendChild(h('p', { className: 'form-hint' }, publicEffective
      ? t('spend.publicTodayOf', { used: fmtInt(s.public_tokens_used_today), limit: fmtInt(publicEffective) })
      : t('spend.publicToday', { used: fmtInt(s.public_tokens_used_today) })));
    if (s.paused_by_limit) {
      body.appendChild(h('p', { className: 'form-hint', style: { color: 'var(--warning)' } }, t('spend.paused')));
    }
    if (s.public_paused_by_limit) {
      body.appendChild(h('p', { className: 'form-hint', style: { color: 'var(--warning)' } }, t('spend.publicPaused')));
    }
    if (s.unpriced_calls_today > 0) {
      body.appendChild(h('p', { className: 'form-hint' },
        t('spend.unpriced', { count: fmtInt(s.unpriced_calls_today) })));
    }

    body.appendChild(formGroup(t('spend.tokenLimit'), tokensInput, {
      hint: t('spend.tokenHint'),
    }));
    body.appendChild(formGroup(t('spend.publicTokenLimit'), publicInput, {
      hint: t('spend.publicTokenHint', { value: fmtInt(s.default_public_token_budget) }),
    }));
    body.appendChild(formGroup(t('spend.usdLimit'), usdInput, {
      hint: t('spend.usdHint'),
    }));

    const save = h('button', { className: 'btn btn--primary btn--sm' }, t('spend.save'));
    save.addEventListener('click', async () => {
      const tokens = readNumber(tokensInput, { integer: true });
      const publicTokens = readNumber(publicInput, { integer: true });
      const usd = readNumber(usdInput, { integer: false });
      if (tokens === undefined || publicTokens === undefined || usd === undefined) {
        toast.warning(t('spend.invalid'));
        return;
      }
      save.disabled = true;
      try {
        const next = await put(`/admin/api/projects/${projectId}/spend-limits`, {
          daily_token_budget: tokens,
          daily_public_token_budget: publicTokens,
          daily_usd_cap: usd,
        });
        toast.success(t('spend.saved'));
        paint(next);
      } catch (err) {
        toast.error(t('set.saveFailed', { error: humanizeError(err.message) }));
        save.disabled = false;
      }
    });
    body.appendChild(h('div', { className: 'mt-3' }, [save]));
  }

  async function load() {
    try {
      paint(await get(`/admin/api/projects/${projectId}/spend-limits`));
    } catch (err) {
      clear(body);
      body.appendChild(h('p', { className: 'form-hint' }, t('spend.loadFailed', { error: humanizeError(err.message) })));
    }
  }
  load();
  return card;
}
