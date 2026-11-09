/**
 * Setup Step 5: Connect AI provider. Two-phase because the model catalog is
 * discovered at sync time rather than baked into seed data: Phase A picks a
 * provider and submits the API key (the server stores it and runs a
 * synchronous ListModels); Phase B picks a model from the now-populated list.
 * Keyless providers (local runtimes) skip the API-key input but still go
 * through Phase A so the sync runs and the model dropdown gets filled.
 */

import { h, clear } from '../../core/dom.js';
import { t, tn } from '../../core/i18n.js';
import { get, post } from '../../core/http.js';
import * as toast from '../../ui/toast.js';
import { signupHint, isCodingPlan } from '../../ui/signup-hint.js';
import { setupAuth } from './setup-auth.js';
import { isSubmitEnter } from '../../ui/a11y.js';
import { humanizeError } from '../../ui/errors.js';

// ctx is the setup orchestrator's step context (see setup/index.js): the
// card body to render into, setters for the card's header and footer, the
// shared wizard data, and next/back.
export async function renderProvider(ctx) {
  const { body: container, setHeader, setActions, data: setupData, next: onNext, back: onBack } = ctx;

  setHeader(t('setup.provider.title'), t('setup.provider.loading'));
  setActions([h('button', { className: 'btn btn--ghost', onClick: onBack }, t('common.back'))]);
  container.appendChild(h('div', { className: 'auth-card__loading' }, [
    h('span', { className: 'spinner' }),
  ]));

  let providers = [];
  try {
    providers = await get('/admin/api/setup/providers', setupAuth(setupData));
  } catch (err) {
    clear(container);
    // 410 Gone = an admin account already exists, so this setup wizard should
    // not be running (e.g. data was reset without clearing the global DB, which
    // still holds the admin user). Don't dead-end on "Failed to load"; tell the
    // user setup is done and reload so boot() routes them to the sign-in screen.
    if (err && err.status === 410) {
      setHeader(t('setup.provider.doneTitle'), t('setup.provider.doneBody'), { why: false });
      setActions([]);
      setTimeout(() => { window.location.reload(); }, 1500);
      return;
    }
    const tokenRejected = err && err.status === 403;
    container.appendChild(h('p', { className: 'text-danger', role: 'alert' },
      tokenRejected ? t('setup.provider.tokenRejected') : t('setup.provider.loadFailed')));
    setActions([
      h('button', { className: 'btn btn--ghost', onClick: onBack }, t('common.back')),
      ...(tokenRejected ? [] : [h('button', {
        className: 'btn btn--primary',
        onClick: () => { clear(container); renderProvider(ctx); },
      }, t('common.tryAgain'))]),
    ]);
    return;
  }

  clear(container);

  if (!providers || providers.length === 0) {
    setHeader(t('setup.provider.noneTitle'), t('setup.provider.noneBody'), { why: false });
    setActions([
      h('button', { className: 'btn btn--ghost', onClick: onBack }, t('common.back')),
      h('button', {
        className: 'btn btn--primary',
        onClick: () => {
          setupData.providerId = 0;
          setupData.providerName = '';
          setupData.modelId = '';
          setupData.apiKey = '';
          onNext();
        },
      }, t('setup.provider.continueWithoutAi')),
    ]);
    return;
  }

  renderConnectStep(ctx, providers);
}

function getProviderBadge(provider) {
  if (!provider.requires_api_key) {
    return h('span', { className: 'provider-badge provider-badge--local' }, t('setup.provider.badgeLocal'));
  }
  if (isCodingPlan(provider)) {
    return h('span', { className: 'provider-badge provider-badge--coding' }, t('setup.provider.badgeCoding'));
  }
  return h('span', { className: 'provider-badge provider-badge--cloud' }, t('setup.provider.badgeCloud'));
}

// isCloudProvider reports whether a provider needs a key and usage from it
// counts against a vendor-side allowance (so the daily ceiling is meaningful).
// Keyless providers run locally and are free.
function isCloudProvider(provider) {
  return !!provider.requires_api_key;
}

// renderConnectStep is Phase A: pick a provider, enter API key, hit
// "Connect". When the provider already has models from a prior boot we
// short-circuit straight into Phase B without bothering the user for a
// key they have already provided.
function renderConnectStep(ctx, providers) {
  const { body: container, setHeader, setActions, data: setupData } = ctx;
  // Own the container, the way renderModelStep does, so a re-render never
  // stacks a second copy of the screen underneath the first.
  clear(container);

  // Restore the user's previous pick when they came back from Phase B,
  // otherwise default to the first provider. Without this the dropdown
  // resets to providers[0] every time renderProvider runs.
  const initialID = setupData.providerId || 0;
  let selectedProvider = providers.find((p) => p.id === initialID) || providers[0];

  const providerSelect = h('select', { className: 'input', id: 'setup-provider' });

  // The seeded rows are curated, and the keyless local runtimes among them
  // need no key at all, which is the fastest possible first run. Coding-plan
  // rows are grouped apart so they are not mistaken for metered APIs.
  const standard = providers.filter((p) => !isCodingPlan(p));
  const coding = providers.filter((p) => isCodingPlan(p));
  const addOptions = (list, group) => list.forEach((p) => {
    const opt = h('option', { value: String(p.id) }, p.name);
    if (p.id === selectedProvider.id) opt.selected = true;
    group.appendChild(opt);
  });
  if (coding.length === 0) {
    addOptions(standard, providerSelect);
  } else {
    const standardGroup = h('optgroup', { label: t('setup.provider.groupProviders') });
    const codingGroup = h('optgroup', { label: t('setup.provider.groupCoding') });
    addOptions(standard, standardGroup);
    addOptions(coding, codingGroup);
    providerSelect.append(standardGroup, codingGroup);
  }

  const badgeContainer = h('span');
  function updateBadge() {
    badgeContainer.innerHTML = '';
    if (selectedProvider) badgeContainer.appendChild(getProviderBadge(selectedProvider));
  }

  const apiKeyInput = h('input', {
    className: 'input',
    id: 'setup-api-key',
    type: 'password',
    placeholder: t('setup.provider.keyPlaceholder'),
    // An API key is not the account password: keep the browser from filling or
    // offering to save it as one.
    autocomplete: 'off',
    autocapitalize: 'off',
    spellcheck: 'false',
  });
  const apiKeyGroup = h('div', { className: 'form-group' });
  const submitBtn = h('button', { className: 'btn btn--primary' }, t('setup.provider.connect'));

  function updateAPIKeyUI() {
    apiKeyGroup.innerHTML = '';
    if (!selectedProvider) return;

    if (!selectedProvider.requires_api_key) {
      apiKeyGroup.appendChild(
        h('p', { className: 'form-hint', style: { color: 'var(--success)' } },
          t('setup.provider.noKeyNeeded'))
      );
      return;
    }

    // Always render an editable field when a key is required. If one is
    // already stored (user came back from the model picker, or env-var
    // seeded it), surface that as a placeholder + hint so the user can
    // either leave it alone or paste a replacement.
    apiKeyGroup.appendChild(h('label', { for: 'setup-api-key' }, [
      t('setup.provider.keyLabel'),
      selectedProvider.has_api_key ? null : h('span', { className: 'required' }, ' *'),
    ]));
    apiKeyInput.placeholder = selectedProvider.has_api_key
      ? t('setup.provider.keyKeepPlaceholder')
      : t('setup.provider.keyPlaceholder');
    apiKeyGroup.appendChild(apiKeyInput);
    if (!selectedProvider.has_api_key) {
      apiKeyGroup.appendChild(
        h('p', { className: 'form-hint form-hint--info' },
          t('setup.provider.keyExplain'))
      );
    }
    if (selectedProvider.has_api_key) {
      apiKeyGroup.appendChild(
        h('p', { className: 'form-hint', style: { color: 'var(--success)' } },
          t('setup.provider.keyStored'))
      );
    } else {
      apiKeyGroup.appendChild(signupHint(selectedProvider.signup_url));
    }
  }

  providerSelect.addEventListener('change', () => {
    const id = parseInt(providerSelect.value, 10);
    selectedProvider = providers.find(p => p.id === id) || null;
    apiKeyInput.value = '';
    updateAPIKeyUI();
    updateBadge();
  });

  apiKeyInput.addEventListener('keydown', (e) => {
    if (isSubmitEnter(e)) {
      e.preventDefault();
      submit();
    }
  });

  async function submit() {
    if (!selectedProvider) {
      toast.warning(t('setup.provider.selectProvider'));
      return;
    }
    const needsKey = selectedProvider.requires_api_key && !selectedProvider.has_api_key;
    const apiKey = apiKeyInput.value.trim();
    if (needsKey && !apiKey) {
      toast.warning(t('setup.provider.keyRequired', { name: selectedProvider.name }));
      apiKeyInput.focus();
      return;
    }
    // Empty string when the user is reusing a stored key, the server
    // detects that and falls back to the encrypted value already on the row.

    submitBtn.disabled = true;
    submitBtn.textContent = t('setup.provider.connecting');
    try {
      const result = await post(`/admin/api/setup/providers/${selectedProvider.id}/sync`, {
        api_key: apiKey,
      }, setupAuth(setupData));
      const models = (result && result.models) || [];
      if (result && result.sync_error && models.length === 0) {
        toast.error(t('setup.provider.modelsFailed', { error: humanizeError(result.sync_error) }));
        submitBtn.disabled = false;
        submitBtn.textContent = t('setup.provider.connect');
        return;
      }
      if (models.length === 0) {
        toast.error(t('setup.provider.noModels'));
        submitBtn.disabled = false;
        submitBtn.textContent = t('setup.provider.connect');
        return;
      }
      // Remember the key so HandleSetup re-encrypts the same value;
      // server-side it's already stored, but the wizard's final POST
      // still expects to see it so the provider row update path stays
      // consistent with the existing flow.
      setupData.apiKey = apiKey;
      setupData.providerId = selectedProvider.id;
      setupData.providerName = selectedProvider.name;
      renderModelStep(ctx, selectedProvider, models);
    } catch (err) {
      toast.error(t('setup.provider.connectFailed', { error: humanizeError(err.message || t('common.unknownError')) }));
      submitBtn.disabled = false;
      submitBtn.textContent = t('setup.provider.connect');
    }
  }

  submitBtn.addEventListener('click', submit);

  updateAPIKeyUI();
  updateBadge();

  setHeader(t('setup.provider.title'), t('setup.provider.lede'));
  container.append(
    h('div', { className: 'form-group' }, [
      h('label', { for: 'setup-provider' }, [t('setup.provider.serviceLabel') + ' ', badgeContainer]),
      providerSelect,
    ]),
    apiKeyGroup,
  );
  // No key yet, or a local runtime that is not running: the wizard must not
  // dead-end here. The account is created without an AI and one can be added
  // later from LLM Providers.
  const skipBtn = h('button', {
    className: 'btn btn--ghost',
    onClick: () => {
      setupData.providerId = 0;
      setupData.providerName = '';
      setupData.modelId = '';
      setupData.apiKey = '';
      ctx.next();
    },
  }, t('setup.provider.skip'));
  setActions([
    h('button', { className: 'btn btn--ghost', onClick: ctx.back }, t('common.back')),
    h('div', { className: 'flex items-center gap-2' }, [skipBtn, submitBtn]),
  ]);
}

// renderModelStep is Phase B: list the models that were just synced and
// let the user pick one. "Back" returns to Phase A so they can change
// provider or key without restarting the wizard.
function renderModelStep(ctx, provider, models) {
  const { body: container, setHeader, setActions, data: setupData, next: onNext } = ctx;
  clear(container);

  let selectedModel = models.find(m => m.is_default) || models[0];
  let filtered = models.slice();

  // Daily spend safety limit. Only meaningful for keyed providers; local
  // providers are free, so we send 0 (unlimited) for them and show nothing. The default matches the safe built-in ceiling.
  const cloud = isCloudProvider(provider);
  let dailyTokenBudget = (setupData.dailyTokenBudget != null) ? setupData.dailyTokenBudget : 5000000;
  const budgetSelect = h('select', { className: 'input' });
  [
    { v: 1000000, label: t('setup.provider.budget1m') },
    { v: 5000000, label: t('setup.provider.budget5m') },
    { v: 0, label: t('setup.provider.budgetNone') },
  ].forEach((o) => {
    const opt = h('option', { value: String(o.v) }, o.label);
    if (o.v === dailyTokenBudget) opt.selected = true;
    budgetSelect.appendChild(opt);
  });
  budgetSelect.addEventListener('change', () => {
    dailyTokenBudget = parseInt(budgetSelect.value, 10) || 0;
  });

  const modelSelect = h('select', { className: 'input' });
  const filterInput = h('input', {
    className: 'input',
    type: 'search',
    placeholder: models.length > 20
      ? t('setup.provider.filterMany', { count: models.length })
      : t('setup.provider.filter'),
    autocomplete: 'off',
  });
  filterInput.classList.add('mb-2');
  const matchHint = h('p', { className: 'form-hint' }, '');
  const continueBtn = h('button', { className: 'btn btn--primary' }, t('common.continue'));

  function renderOptions() {
    modelSelect.innerHTML = '';
    if (filtered.length === 0) {
      modelSelect.appendChild(h('option', { disabled: true, selected: true }, t('setup.provider.noMatchOption')));
      modelSelect.disabled = true;
      continueBtn.disabled = true;
      matchHint.textContent = t('setup.provider.noMatchHint');
      matchHint.style.color = 'var(--danger)';
      return;
    }
    modelSelect.disabled = false;
    continueBtn.disabled = false;
    // Preserve current selection when still visible; otherwise fall back
    // to the first visible row so Continue always has something to send.
    if (!filtered.includes(selectedModel)) {
      selectedModel = filtered[0];
    }
    filtered.forEach((m) => {
      const opt = h('option', { value: m.model_id }, m.display_name);
      if (m === selectedModel) opt.selected = true;
      modelSelect.appendChild(opt);
    });
    matchHint.textContent = filtered.length === models.length
      ? tn('setup.provider.available', models.length)
      : t('setup.provider.matches', { shown: filtered.length, total: models.length });
    matchHint.style.color = '';
  }

  modelSelect.addEventListener('change', () => {
    selectedModel = filtered.find(m => m.model_id === modelSelect.value) || null;
  });

  filterInput.addEventListener('keydown', (e) => {
    if (isSubmitEnter(e)) {
      e.preventDefault();
      submit();
    }
  });

  filterInput.addEventListener('input', () => {
    const q = filterInput.value.trim().toLowerCase();
    if (!q) {
      filtered = models.slice();
    } else {
      filtered = models.filter((m) =>
        (m.display_name || '').toLowerCase().includes(q) ||
        (m.model_id || '').toLowerCase().includes(q)
      );
    }
    renderOptions();
  });

  function submit() {
    if (!selectedModel) {
      toast.warning(t('setup.provider.pickModel'));
      return;
    }
    setupData.modelId = selectedModel.model_id;
    setupData.dailyTokenBudget = cloud ? dailyTokenBudget : 0;
    onNext();
  }

  continueBtn.addEventListener('click', submit);
  renderOptions();

  setHeader(t('setup.provider.pickTitle'),
    tn('setup.provider.connected', models.length, { name: provider.name }));
  container.append(
    h('div', { className: 'form-group' }, [
      h('label', {}, [t('setup.provider.modelLabel'), h('span', { className: 'required' }, ' *')]),
      ...(models.length > 10 ? [filterInput] : []),
      modelSelect,
      matchHint,
    ]),
    ...(cloud ? [h('div', { className: 'form-group' }, [
      h('label', {}, t('setup.provider.budgetLabel')),
      budgetSelect,
      h('p', { className: 'form-hint form-hint--info' }, t('setup.provider.budgetHint')),
    ])] : []),
  );
  setActions([
    h('button', {
      className: 'btn btn--ghost',
      onClick: () => {
        // Reload from scratch so the list reflects the row state
        // (has_api_key flipped to true after the sync above).
        clear(container);
        renderProvider(ctx);
      },
    }, t('common.back')),
    continueBtn,
  ]);
}
