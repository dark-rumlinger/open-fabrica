/**
 * Setup Step 6: Launch animation with animated checklist.
 */

import { h, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { icon } from '../../ui/icon.js';
import { post } from '../../core/http.js';
import { setupAuth } from './setup-auth.js';
import * as toast from '../../ui/toast.js';
import { humanizeError } from '../../ui/errors.js';

export function renderLaunching({ body: container, setHeader, setActions, data: setupData }) {
  const checklist = [
    { key: 'account', label: t('setup.launch.account') },
    { key: 'provider', label: t('setup.launch.provider') },
    { key: 'ready', label: t('setup.launch.ready') },
  ];

  // Keyed by role so the run logic never depends on a line's position.
  const itemByKey = {};
  const items = checklist.map((step) => {
    const itemIcon = h('div', { className: 'launch-item__icon' });
    const el = h('div', { className: 'launch-item' }, [
      itemIcon,
      h('span', {}, step.label),
    ]);
    const rec = { el, itemIcon };
    itemByKey[step.key] = rec;
    return rec;
  });

  const title = setupData.displayName
    ? t('setup.launch.titleName', { name: setupData.displayName })
    : t('setup.launch.title');

  setHeader(title, t('setup.launch.lede'));
  container.appendChild(h('div', { className: 'launch-checklist' }, items.map(i => i.el)));

  // Animated step progression. Each helper takes a checklist record and
  // no-ops when it's undefined (a step that isn't part of this run).
  async function runSetup() {
    function markActive(rec) {
      if (!rec) return;
      rec.el.style.color = '';
      rec.el.classList.add('active');
      rec.itemIcon.innerHTML = `<span class="spinner spinner--sm"></span>`;
    }

    function markDone(rec) {
      if (!rec) return;
      rec.el.classList.remove('active');
      rec.el.classList.add('done');
      rec.itemIcon.innerHTML = icon('check');
    }

    function markError(rec) {
      if (!rec) return;
      rec.el.style.color = 'var(--danger)';
      rec.itemIcon.innerHTML = icon('x');
    }

    function markSkipped(rec) {
      if (!rec) return;
      rec.el.classList.remove('active');
      rec.el.style.color = 'var(--text-tertiary)';
      rec.itemIcon.innerHTML = `<span class="text-tertiary">&middot;</span>`;
    }

    // The step currently running, so the catch block flags the right line.
    let errorItem = itemByKey.account;

    // A retry starts the checklist over from a clean slate.
    setActions([]);
    for (const rec of items) {
      rec.el.classList.remove('active', 'done');
      rec.el.style.color = '';
      rec.itemIcon.innerHTML = '';
    }

    try {
      // Create admin account + optional provider
      errorItem = itemByKey.account;
      markActive(itemByKey.account);
      await delay(500);

      const setupPayload = {
        username: setupData.username,
        password: setupData.password,
        display_name: setupData.displayName || '',
      };

      if (setupData.providerId) {
        setupPayload.provider_id = setupData.providerId;
        if (setupData.modelId) setupPayload.model_id = setupData.modelId;
        if (setupData.apiKey) setupPayload.api_key = setupData.apiKey;
      }

      // Persist the chosen daily spend ceiling (0 = unlimited, e.g. local
      // providers). Sent as a number so the server can distinguish it from
      // "not chosen" (absent) and keep the resolved default in that case.
      if (setupData.dailyTokenBudget != null) {
        setupPayload.engine_daily_token_budget = setupData.dailyTokenBudget;
      }

      const result = await post('/admin/api/setup', setupPayload, setupAuth(setupData));
      // Storage can be blocked (private mode, policy). The account exists
      // either way, so a failed write must not read as a failed setup: the
      // owner just signs in at the redirect.
      try {
        localStorage.setItem('of_token', result.token);
        localStorage.setItem('of_user', JSON.stringify(result.user));
      } catch { /* sign in after the redirect */ }
      markDone(itemByKey.account);

      // Provider
      errorItem = itemByKey.provider;
      markActive(itemByKey.provider);
      await delay(600);
      if (setupData.providerId) {
        markDone(itemByKey.provider);
      } else {
        markSkipped(itemByKey.provider);
      }

      // Ready, show celebration
      errorItem = itemByKey.ready;
      markActive(itemByKey.ready);
      await delay(400);
      markDone(itemByKey.ready);

      await delay(600);

      // Replace checklist with the reverent finale: the mark settles back in
      // and glows once, bookending the opening splash.
      clear(container);
      setHeader(setupData.displayName
        ? t('setup.launch.doneName', { name: setupData.displayName })
        : t('setup.launch.done'), null);
      container.appendChild(h('div', { className: 'launch-finale' }, [
        h('div', { className: 'launch-finale__mark' }, [
          h('img', {
            className: 'launch-finale__logo',
            src: '/openfabrica_logo.svg',
            alt: 'Open Fabrica',
          }),
        ]),
        h('div', { className: 'launch-finale__sub' }, t('setup.launch.next')),
        h('div', { className: 'launch-finale__sub launch-finale__status' }, t('setup.launch.redirecting')),
      ]));

      await delay(1600);
      // Full load: the app shell has not been built yet in this tab. The
      // composer is where the first project actually gets made.
      window.location.href = '/new';
    } catch (err) {
      // 410: setup already completed. The first request landed but its reply
      // was lost, so the account exists; retrying can only repeat this answer.
      if (err && err.status === 410) {
        toast.success(t('setup.launch.alreadyDone'));
        window.location.href = '/login';
        return;
      }
      markError(errorItem);
      toast.error(t('setup.launch.failed', { error: humanizeError(err.message) }));
      // Not a dead end: the wizard data is intact, so running it again is
      // safe. (Going back is not offered: the account may already exist.)
      setActions([
        h('button', { className: 'btn btn--primary', onClick: runSetup }, t('common.tryAgain')),
      ]);
    }
  }

  runSetup();
}

function delay(ms) {
  return new Promise(resolve => setTimeout(resolve, ms));
}
