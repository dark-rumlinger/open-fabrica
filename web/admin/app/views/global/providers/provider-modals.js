import { h } from '../../../core/dom.js';
import { t } from '../../../core/i18n.js';
import { get, post, put, del } from '../../../core/http.js';
import * as toast from '../../../ui/toast.js';
import * as modal from '../../../ui/modal.js';
import { formGroup } from '../../../ui/helpers.js';
import { signupHint } from '../../../ui/signup-hint.js';
import { humanizeError } from '../../../ui/errors.js';
import { runAction } from '../../../ui/actions.js';
import { createFormModal } from '../../../ui/form-modal.js';

export async function testProvider(id) {
  try {
    toast.info(t('pm.testing'));
    const result = await post(`/admin/api/providers/${id}/test`);
    if (result.success && result.exercised === false) {
      // Image, stock and search providers have no cheap probe: say so rather
      // than claim a connection that was never made.
      toast.info(t('pm.notVerified'));
    } else if (result.success) {
      toast.success(t('pm.connectionOk'));
    } else {
      toast.error(t('pm.connectionFailed', { error: humanizeError(result.error || t('common.unknownError')) }));
    }
  } catch (err) {
    toast.error(t('pm.testFailed', { error: humanizeError(err.message) }));
  }
}

// syncModels pulls the provider's live catalog and upserts llm_models rows.
// New models are inserted disabled, admins flip the ones they actually
// want into the picker. Surfaces the added/updated counts in the toast so
// the action feels grounded.
export async function syncModels(id, reload) {
  try {
    toast.info(t('pm.syncing'));
    const result = await post(`/admin/api/providers/${id}/sync-models`);
    if (result && result.success) {
      toast.success(t('pm.synced', { discovered: result.discovered, added: result.added, updated: result.updated }));
      if (reload) reload();
    } else {
      toast.error(t('pm.syncFailed', { error: humanizeError((result && result.error) || t('common.unknownError')) }));
    }
  } catch (err) {
    toast.error(t('pm.syncFailed', { error: humanizeError(err.message) }));
  }
}

// checkCapabilities probes every enabled model of a provider for what its listing
// does not report (reasoning, tool use). It stops early when the provider starts
// rate limiting; the toast says how far it got.
export async function checkCapabilities(id, reload) {
  try {
    toast.info(t('pm.checking'));
    const result = await post(`/admin/api/providers/${id}/probe-models`);
    if (result.rate_limited) {
      toast.error(t('pm.checkStopped', { checked: result.checked, total: result.total }));
    } else {
      toast.success(t('pm.checked', { checked: result.checked }));
    }
    if (reload) reload();
  } catch (err) {
    toast.error(humanizeError(err.message));
  }
}

// setDefaultProvider flips is_default on for this provider (clearing
// other providers of the same kind). Used by the image/stock provider
// cards, surfaces a small toast + reload so the new "Default" badge
// appears immediately.
export async function setDefaultProvider(id, reload) {
  try {
    await post(`/admin/api/providers/${id}/default`);
    toast.success(t('pm.defaultUpdated'));
    if (reload) reload();
  } catch (err) {
    toast.error(t('pm.defaultFailed', { error: humanizeError(err.message) }));
  }
}

// syncAllProviders fans out to /sync-all and surfaces a single rollup
// toast plus per-provider error toasts so a partial failure is visible
// without overwhelming the admin.
export async function syncAllProviders(reload) {
  try {
    toast.info(t('pm.syncingAll'));
    const result = await post(`/admin/api/providers/sync-all`);
    const rows = (result && result.providers) || [];
    let added = 0, updated = 0, discovered = 0, errors = 0;
    rows.forEach((r) => {
      if (r.error) { errors++; toast.error(`${r.name}: ${humanizeError(r.error)}`); return; }
      added += r.added; updated += r.updated; discovered += r.discovered;
    });
    if (errors === 0) toast.success(t('pm.syncedAll', { providers: rows.length, discovered, added, updated }));
    else toast.info(t('pm.syncedPartial', { ok: rows.length - errors, failed: errors }));
    if (reload) reload();
  } catch (err) {
    toast.error(t('pm.syncAllFailed', { error: humanizeError(err.message) }));
  }
}

function attachInlineTest(getValues) {
  // role=status so the result is announced when it lands.
  const status = h('span', { className: 'form-hint', role: 'status', style: { marginInlineStart: '0.5rem' } });
  const btn = h('button', { className: 'btn btn--sm', type: 'button' }, t('pm.testConnection'));
  btn.addEventListener('click', async () => {
    const v = getValues();
    btn.disabled = true;
    status.textContent = t('pm.inlineTesting');
    status.style.color = '';
    try {
      const result = await post('/admin/api/providers/test', {
        transport: v.transport,
        api_key: v.apiKey,
        base_url: v.baseUrl,
        // A blank key on an existing provider tests the key already saved.
        provider_id: v.providerId || undefined,
      });
      if (result.success && result.exercised === false) {
        status.textContent = t('pm.inlineNotVerified');
        status.style.color = 'var(--text-secondary)';
      } else if (result.success) {
        status.textContent = t('pm.inlineOk');
        status.style.color = 'var(--success)';
      } else {
        status.textContent = t('pm.inlineFailed', { error: result.error || t('common.unknownError') });
        status.style.color = 'var(--danger)';
      }
    } catch (err) {
      status.textContent = t('pm.inlineFailed', { error: humanizeError(err) });
      status.style.color = 'var(--danger)';
    } finally {
      btn.disabled = false;
    }
  });
  return h('div', { className: 'mt-2 flex items-center' }, [btn, status]);
}

// showAddProviderModal opens the "Add Provider" dialog for a custom endpoint of
// the given kind ('llm', 'image', 'stock', 'search'). Pass null/undefined to
// show every kind.
//
// The seeded providers are created at boot, so this dialog is the escape hatch
// for everything else: a self-hosted server, a private gateway, or any vendor
// that speaks one of the supported wire protocols. The options come from
// /providers/transports, the protocols this build speaks.
export async function showAddProviderModal(kind, reload) {
  // Back-compat: callers that pre-date the kind argument pass (reload) as
  // the first positional, treat them as "no filter".
  if (typeof kind === 'function' && reload === undefined) {
    reload = kind;
    kind = null;
  }

  const q = kind ? `?kind=${encodeURIComponent(kind)}` : '';
  let types;
  try {
    types = await get('/admin/api/providers/transports' + q);
  } catch (err) {
    toast.error(t('pm.typesFailed', { error: humanizeError(err.message) }));
    return;
  }
  if (!types || types.length === 0) {
    toast.error(t('pm.noTypes'));
    return;
  }

  // Each option carries everything the form needs, so the change handler
  // never has to look anything back up.
  const options = types.map((ty) => ({
    key: 'transport:' + ty.transport,
    label: ty.label,
    transport: ty.transport,
    name: ty.label,
    baseURL: ty.default_base_url || '',
    modelsURL: ty.default_models_url || '',
    showBaseURL: ty.show_base_url,
    baseURLRequired: ty.base_url_required,
    requiresAPIKey: ty.requires_api_key,
  }));

  let selected = options[0];
  // Names we auto-filled; lets us replace the name on change only while the
  // admin has not typed their own.
  const autoNames = new Set(options.map((o) => o.name));

  const nameInput = h('input', { className: 'input', value: selected.name, placeholder: t('pm.namePlaceholder') });
  // autocomplete off: this is a vendor API key, not the admin's own password,
  // so the browser must not fill it or offer to save it.
  const apiKeyInput = h('input', { className: 'input', type: 'password', placeholder: t('pm.keyPlaceholder'), autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false' });
  const baseUrlInput = h('input', { className: 'input', placeholder: 'https://api.example.com/v1' });

  const typeSelect = h('select', { className: 'input' },
    options.map((o) => h('option', { value: o.key }, o.label)));

  const baseUrlGroup = formGroup(t('sp.baseUrl'), baseUrlInput, {
    required: true,
    hint: t('pm.baseUrlHint'),
  });
  const apiKeyGroup = formGroup(t('setup.provider.keyLabel'), apiKeyInput, { required: true });
  apiKeyGroup.appendChild(attachInlineTest(() => ({
    transport: selected.transport,
    apiKey: apiKeyInput.value.trim(),
    baseUrl: baseUrlInput.value.trim(),
  })));

  const applyType = () => {
    if (!nameInput.value || autoNames.has(nameInput.value)) {
      nameInput.value = selected.name;
    }
    baseUrlInput.value = selected.baseURL || '';
    baseUrlGroup.style.display = selected.showBaseURL ? '' : 'none';
    apiKeyGroup.style.display = selected.requiresAPIKey ? '' : 'none';
  };

  typeSelect.addEventListener('change', () => {
    selected = options.find((o) => o.key === typeSelect.value) || options[0];
    applyType();
  });
  applyType();

  createFormModal({
    title: t('pm.addTitle'),
    fields: [
      formGroup(t('sp.provider'), typeSelect, { required: true }),
      formGroup(t('projects.col.name'), nameInput, { required: true }),
      baseUrlGroup,
      apiKeyGroup,
    ],
    submitLabel: t('pm.addTitle'),
    validate: () => {
      if (!nameInput.value.trim()) return t('pm.nameRequired');
      if (selected.baseURLRequired && !baseUrlInput.value.trim()) {
        return t('pm.baseUrlRequiredFor', { label: selected.label });
      }
      // The label promises a key; without one the row would be created only to
      // sit there reading "Needs API key".
      if (selected.requiresAPIKey && !apiKeyInput.value.trim()) return t('pm.keyRequired');
      return null;
    },
    // vendor is omitted: the backend slugifies one from the name. kind is
    // always omitted too, the backend derives it from the transport and stays
    // the single authority for it.
    submit: () => post('/admin/api/providers', {
      name: nameInput.value.trim(),
      transport: selected.transport,
      models_url: selected.modelsURL || '',
      api_key: selected.requiresAPIKey ? apiKeyInput.value.trim() : '',
      base_url: baseUrlInput.value.trim() || null,
    }),
    success: t('pm.added'),
    onDone: reload,
  });
}

// Pretty-prints a provider's stored config for the advanced editor. Returns ''
// for an empty config so the textarea opens blank rather than showing '{}',
// which reads like something is configured when nothing is.
function formatConfig(cfg) {
  if (!cfg || typeof cfg !== 'object' || Object.keys(cfg).length === 0) return '';
  return JSON.stringify(cfg, null, 2);
}

export function showEditProviderModal(provider, reload) {
  const nameInput = h('input', { className: 'input', value: provider.name });
  const apiKeyInput = h('input', { className: 'input', type: 'password', placeholder: t('pm.keepKeyPlaceholder'), autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false' });
  const baseUrlInput = h('input', {
    className: 'input',
    value: provider.base_url || '',
    placeholder: 'https://api.example.com/v1',
  });
  const enabledCheck = h('input', { type: 'checkbox', checked: provider.is_enabled !== false });

  // Advanced: the row's providercfg blob. This is the escape hatch for a vendor
  // whose credential does not go in an Authorization: Bearer header, or whose
  // model listing is shaped differently - the things that would otherwise require a Go
  // package per vendor. Edited as JSON because it is rare and structured; the
  // server validates it and returns a specific message ("auth: scheme
  // \"header\" requires a header name") rather than accepting a typo.
  const originalConfig = formatConfig(provider.config);
  const configInput = h('textarea', {
    className: 'input',
    rows: 6,
    spellcheck: false,
    style: { fontFamily: 'var(--font-mono, monospace)', fontSize: '0.8rem' },
    placeholder: '{\n  "auth": { "scheme": "header", "header": "X-Api-Key" }\n}',
  });
  configInput.value = originalConfig;
  // Base URL / API-key behavior rides on the provider row (derived
  // server-side from the transport), so the frontend keeps no
  // per-transport table of its own.
  const showBaseUrl = provider.show_base_url !== false;
  const baseUrlRequired = provider.base_url_required === true;
  const showAPIKey = provider.requires_api_key !== false;

  const apiKeyGroup = showAPIKey
    ? formGroup(t('setup.provider.keyLabel'), apiKeyInput, { hint: t('pm.keepKeyHint') })
    : null;
  if (apiKeyGroup) {
    apiKeyGroup.appendChild(signupHint(provider.signup_url || ''));
    apiKeyGroup.appendChild(attachInlineTest(() => ({
      transport: provider.transport,
      apiKey: apiKeyInput.value.trim(),
      baseUrl: (baseUrlInput.value && baseUrlInput.value.trim()) || provider.base_url || '',
      providerId: provider.id,
    })));
  }

  const advanced = h('details', { className: 'form-group' }, [
    h('summary', { className: 'text-sm', style: { cursor: 'pointer' } }, t('tabs.advanced')),
    formGroup(t('pm.configLabel'), configInput, {
      hint: t('pm.configHint'),
    }),
  ]);

  createFormModal({
    title: t('pm.editTitle'),
    fields: [
      formGroup(t('projects.col.name'), nameInput, { required: true }),
      ...(showBaseUrl ? [formGroup(t('sp.baseUrl'), baseUrlInput, {
        required: baseUrlRequired,
        hint: t('pm.baseUrlHint'),
      })] : []),
      ...(apiKeyGroup ? [apiKeyGroup] : []),
      h('div', { className: 'form-group' }, [
        h('label', { className: 'toggle' }, [enabledCheck, t('common.enabled')]),
        h('p', { className: 'form-hint' }, t('pm.enabledHint')),
      ]),
      advanced,
    ],
    validate: () => {
      if (baseUrlRequired && !baseUrlInput.value.trim()) {
        return t('pm.baseUrlRequired');
      }
      // Catch malformed JSON here so the user sees it against the field they
      // just edited. Everything beyond well-formedness (unknown auth scheme, a
      // header scheme with no header name) is the server's call, and its error
      // comes back through the normal submit path.
      const raw = configInput.value.trim();
      if (raw) {
        try {
          const parsed = JSON.parse(raw);
          if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
            return t('pm.configObject');
          }
        } catch (err) {
          return t('pm.configInvalid', { error: err.message });
        }
      }
      return null;
    },
    submit: () => {
      const body = {
        name: nameInput.value.trim(),
        is_enabled: enabledCheck.checked,
        base_url: baseUrlInput.value.trim() || null,
      };
      if (showAPIKey && apiKeyInput.value.trim()) body.api_key = apiKeyInput.value.trim();
      // Send config only when it actually changed: omitting it leaves the
      // column alone, so an admin flipping "Enabled" never rewrites options
      // they did not touch.
      const raw = configInput.value.trim();
      if (raw !== originalConfig.trim()) {
        body.config = raw ? JSON.parse(raw) : {};
      }
      return put(`/admin/api/providers/${provider.id}`, body);
    },
    success: t('pm.updated'),
    onDone: reload,
  });
}

export function confirmDeleteProvider(provider, reload) {
  modal.confirmDanger(
    t('pm.deleteTitle'),
    t('pm.deleteBody', { name: provider.name }),
    () => runAction(() => del(`/admin/api/providers/${provider.id}`), { success: t('pm.deleted'), onDone: reload }),
  );
}
