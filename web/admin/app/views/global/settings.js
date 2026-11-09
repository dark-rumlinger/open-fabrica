/**
 * System settings editor.
 */

import { h, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { get, put, download } from '../../core/http.js';
import { defineView } from '../../core/view.js';
import * as toast from '../../ui/toast.js';
import * as state from '../../core/state.js';
import { pageHeader, pageBody } from '../../ui/card.js';
import { formGroup, emptyState, errorState, copyText, listSkeleton } from '../../ui/helpers.js';
import { humanizeError } from '../../ui/errors.js';
import { wireSubmit } from '../../ui/form.js';
import { systemStatusCard, firewallCard } from './system-cards.js';
import { scheduledBackupsCard } from './backups-card.js';

// The editable system settings, with friendly labels and descriptions. A setting
// shows on this page only if it has an entry here: add one when a new
// operator-facing setting is introduced.
const SETTING_LABELS = {
  engine_daily_token_budget: {
    label: t('settings.budget.label'),
    hint: t('settings.budget.hint'),
    finePrint: t('settings.budget.finePrint'),
  },
  admin_base_url: {
    label: t('settings.adminUrl.label'),
    hint: t('settings.adminUrl.hint'),
  },
  // Add friendly entries here, e.g.:
  // public_port: { label: 'Public port', hint: 'TCP port that visitors connect to.' },
};

function friendlyLabel(key) {
  if (SETTING_LABELS[key]) return SETTING_LABELS[key].label;
  return key.replace(/_/g, ' ').replace(/\b\w/g, c => c.toUpperCase());
}

function friendlyHint(key) {
  return SETTING_LABELS[key]?.hint || null;
}

export const renderSettings = defineView(async (ctx, container) => {
  clear(container);

  const header = pageHeader({
    title: t('nav.settings'),
  });

  const formContainer = pageBody();

  container.appendChild(header);
  container.appendChild(formContainer);

  await loadSettings();

  async function loadSettings() {
    let settings;
    try {
      settings = await get('/admin/api/settings');
    } catch (err) {
      // A load failure used to leave the panel blank behind a toast; show a
      // retryable error state like every other view instead.
      clear(formContainer);
      formContainer.appendChild(errorState({
        message: t('settings.loadFailed', { error: humanizeError(err.message) }),
        retry: loadSettings,
        reference: err.requestId,
      }));
      return;
    }
    if (ctx.scope.disposed) return;
    // Always surface the spend limit even when it currently lives in config or
    // the built-in default (no DB row yet), so it's editable from here.
    if (settings.engine_daily_token_budget == null) {
      settings.engine_daily_token_budget = '';
    }
    if (settings.admin_base_url == null) {
      settings.admin_base_url = '';
    }
    renderForm(settings);
  }

  function renderForm(settings) {
    clear(formContainer);
    const readOnly = !state.isAdmin();

    // Only settings with a label here are editable. The table also holds keys that
    // other pages own (the router port mapping toggle, bot state), and a raw text
    // box over those could overwrite a change made elsewhere.
    const otherKeys = Object.keys(SETTING_LABELS).filter(k => k in settings);
    // When the env var is set it outranks the saved value, so say so beside the field.
    const budgetEnv = settings.engine_daily_token_budget_env;
    const otherFields = {};

    let settingsCard = null;
    if (otherKeys.length > 0) {
      settingsCard = h('div', { className: 'card' }, [
        h('h4', { className: 'card__title mb-4' }, t('tabs.settings')),
      ]);
      for (const key of otherKeys) {
        const input = h('input', {
          className: 'input',
          value: settings[key] || '',
          disabled: readOnly,
        });
        otherFields[key] = input;
        let hint = friendlyHint(key);
        if (key === 'engine_daily_token_budget' && budgetEnv !== undefined) {
          hint = t('settings.budget.envOverride', { value: budgetEnv === '0' ? t('settings.unlimited') : budgetEnv }) + ' ' + (hint || '');
        }
        settingsCard.appendChild(formGroup(friendlyLabel(key), input, { hint }));
        // Billing mechanics live behind a disclosure so the everyday hint
        // stays one readable sentence.
        const finePrint = SETTING_LABELS[key]?.finePrint;
        if (finePrint) {
          settingsCard.appendChild(h('details', { className: 'mb-3' }, [
            h('summary', { className: 'form-hint', style: { cursor: 'pointer' } }, t('settings.howCounted')),
            h('p', { className: 'form-hint', style: { margin: '0.35rem 0 0' } }, finePrint),
          ]));
        }
      }
    }

    // --- Save button ---
    const saveBtn = h('button', { className: 'btn btn--primary btn--sm' }, t('settings.save'));
    const save = wireSubmit(null, saveBtn, {
      busyLabel: t('common.saving'),
      errorPrefix: t('settings.saveFailed'),
      onSubmit: async () => {
        const payload = {};
        for (const [key, input] of Object.entries(otherFields)) {
          const val = input.value.trim();
          // A blank field clears a value that was set; one that was never set stays unsent.
          if (val || settings[key]) payload[key] = val;
        }
        await put('/admin/api/settings', payload);
        toast.success(t('settings.saved'));
      },
    });
    saveBtn.addEventListener('click', save.submit);

    // The save button sits inside the settings panel, under its fields, rather
    // than on its own line below the card.
    if (settingsCard && state.isAdmin()) {
      settingsCard.appendChild(h('div', { className: 'mt-4' }, [saveBtn]));
    }

    if (settingsCard) {
      formContainer.appendChild(settingsCard);
    } else {
      formContainer.appendChild(emptyState({
        iconName: 'settings',
        title: t('settings.empty.title'),
        description: t('settings.empty.description'),
      }));
    }

    if (state.isAdmin()) {
      formContainer.appendChild(backupCard());
      formContainer.appendChild(scheduledBackupsCard());
      formContainer.appendChild(logsCard());
      formContainer.appendChild(configCard());
      formContainer.appendChild(systemStatusCard());
      formContainer.appendChild(firewallCard());
    }
  }
});

// Whole-instance backup download. Restore is a stopped-server CLI step because it
// replaces files the running server holds open.
function backupCard() {
  const btn = h('button', { className: 'btn btn--secondary' }, t('settings.backup.download'));
  const save = wireSubmit(null, btn, {
    busyLabel: t('settings.backup.preparing'),
    errorPrefix: t('settings.backup.failed'),
    onSubmit: async () => {
      await download('/admin/api/system/backup', 'openfabrica-backup.zip');
      toast.success(t('settings.backup.done'));
    },
  });
  btn.addEventListener('click', save.submit);
  return h('div', { className: 'card mt-4' }, [
    h('h4', { className: 'card__title mb-2' }, t('settings.backup.title')),
    h('p', { className: 'form-hint' }, t('settings.backup.body')),
    h('p', { className: 'form-hint' }, t('settings.backup.restore')),
    h('div', { className: 'mt-3' }, [btn]),
  ]);
}

// Read-only view of the configuration the server is running with. These live in
// config.json or environment variables and need a restart to change, so they are
// shown, not edited, to answer "what is this instance actually doing".
function configCard() {
  const body = h('div', {}, [listSkeleton(2)]);

  const list = (a) => (a && a.length ? a.join(', ') : t('settings.config.none'));
  const rows = (c) => [
    [t('settings.config.adminPanel'), `${c.admin_bind_addr}:${c.admin_port} ${c.admin_exposed ? t('settings.config.reachableNetwork') : t('settings.config.thisComputerOnly')}`],
    [t('settings.config.publicPort'), String(c.public_port)],
    [t('settings.config.dataFolder'), c.data_dir],
    [t('settings.config.dbFile'), c.db_path],
    [t('settings.config.logLevel'), c.log_file ? t('settings.config.logLevelFile', { level: c.log_level, file: c.log_file }) : c.log_level],
    [t('settings.config.https'), c.tls_disabled ? t('settings.config.off') : (c.tls_staging ? t('settings.config.onStaging') : t('settings.config.on'))],
    [t('settings.config.csp'), c.public_csp],
    [t('settings.config.cors'), c.cors_origins.length ? list(c.cors_origins) : t('settings.config.anyOrigin')],
    [t('settings.config.proxies'), list(c.trusted_proxies)],
    [t('settings.config.rateLimit'), t('settings.config.rateLimitValue', { rate: c.rate_limit_rate, burst: c.rate_limit_burst })],
    [t('settings.config.timeout'), c.llm_timeout_sec ? `${c.llm_timeout_sec}s` : t('settings.config.builtInDefault')],
    [t('settings.config.budget'), c.engine_daily_token_budget ? String(c.engine_daily_token_budget) : t('settings.config.budgetDefault')],
    [t('settings.config.stages'), t('settings.config.stagesValue', {
      expand: c.stage_timeouts_min.expand || t('settings.config.default'),
      plan: c.stage_timeouts_min.plan || t('settings.config.default'),
      build: c.stage_timeouts_min.build || t('settings.config.default'),
    })],
  ];

  get('/admin/api/system/config').then((c) => {
    clear(body);
    body.appendChild(h('table', { className: 'table text-sm' }, [
      h('tbody', {}, rows(c).map(([k, v]) => h('tr', {}, [
        h('td', { className: 'text-secondary', style: { whiteSpace: 'nowrap', paddingInlineEnd: '16px' } }, k),
        h('td', { style: { wordBreak: 'break-all' } }, v),
      ]))),
    ]));
  }).catch((err) => {
    clear(body);
    body.appendChild(h('p', { className: 'form-hint' }, t('settings.config.loadFailed', { error: humanizeError(err.message) })));
  });

  return h('div', { className: 'card mt-4' }, [
    h('h4', { className: 'card__title mb-2' }, t('settings.config.title')),
    h('p', { className: 'form-hint' }, t('settings.config.readOnly')),
    body,
  ]);
}

// Recent server log lines from the in-memory ring, for diagnosing a problem
// without shell access to the machine.
function logsCard() {
  const pre = h('pre', {
    className: 'text-xs',
    style: { maxHeight: '320px', overflow: 'auto', whiteSpace: 'pre-wrap', wordBreak: 'break-word', margin: '0' },
  });
  // A skeleton until the first load lands, then swapped for the log view in place.
  const box = h('div', {}, [listSkeleton(1)]);
  let text = '';
  let shown = false;

  async function load() {
    try {
      const res = await get('/admin/api/system/logs?lines=300');
      text = (res.lines || []).join('\n');
      pre.textContent = text || t('settings.logs.none');
      if (!shown) { clear(box); box.appendChild(pre); shown = true; }
      pre.scrollTop = pre.scrollHeight;
    } catch (err) {
      toast.error(t('settings.logs.loadFailed', { error: humanizeError(err.message) }));
    }
  }

  const refreshBtn = h('button', { className: 'btn btn--secondary btn--sm', onClick: load }, t('common.refresh'));
  const copyBtn = h('button', { className: 'btn btn--ghost btn--sm', onClick: () => copyText(text, t('settings.logs.copied')) }, t('common.copy'));
  const downloadBtn = h('button', { className: 'btn btn--ghost btn--sm', onClick: () => downloadText(text, 'openfabrica-logs.txt') }, t('settings.logs.download'));
  load();
  return h('div', { className: 'card mt-4' }, [
    h('h4', { className: 'card__title mb-2' }, t('settings.logs.title')),
    h('p', { className: 'form-hint' }, t('settings.logs.hint')),
    h('div', { className: 'flex gap-2 mt-3 mb-2' }, [refreshBtn, copyBtn, downloadBtn]),
    box,
  ]);
}

// Save a string as a downloaded text file. The logs come back as JSON, so there
// is no server file to GET through core/http download().
function downloadText(text, filename) {
  const url = URL.createObjectURL(new Blob([text || ''], { type: 'text/plain' }));
  const a = h('a', { href: url, download: filename });
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}
