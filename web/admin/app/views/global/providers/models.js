/**
 * Per-provider model list + add/edit/delete modals. Owns the inline expander
 * UI under each provider card.
 */

import { h, clear } from '../../../core/dom.js';
import { t } from '../../../core/i18n.js';
import { formatCompact, formatPrice as formatUsdPrice } from '../../../core/format.js';
import { get, post, put, del } from '../../../core/http.js';
import { icon } from '../../../ui/icon.js';
import * as toast from '../../../ui/toast.js';
import * as modal from '../../../ui/modal.js';
import * as state from '../../../core/state.js';
import { formGroup } from '../../../ui/helpers.js';
import { humanizeError } from '../../../ui/errors.js';

export async function loadModels(provider, container) {
  clear(container);
  container.appendChild(h('p', { className: 'text-secondary text-sm' }, t('models.loading')));
  try {
    const modelsList = await get(`/admin/api/providers/${provider.id}/models`);
    renderModels(provider, modelsList, container);
  } catch (err) {
    clear(container);
    container.appendChild(h('p', { className: 'text-danger text-sm' }, t('models.loadFailed', { error: err.message })));
  }
}

function renderModels(provider, modelsList, container) {
  clear(container);

  const headerItems = [h('h5', { className: 'text-sm font-medium' }, t('prov.models'))];
  if (state.isAdmin()) {
    headerItems.push(h('button', {
      className: 'btn btn--sm btn--primary',
      onClick: () => showAddModelModal(provider, container),
    }, [h('span', { innerHTML: icon('plus') }), t('models.add')]));
  }
  container.appendChild(h('div', { className: 'flex items-center justify-between mt-4 mb-2' }, headerItems));

  if (!modelsList?.length) {
    container.appendChild(h('p', { className: 'text-secondary text-sm ml-2' }, t('models.none')));
    return;
  }

  const table = h('div', {
    style: { border: '1px solid var(--border)', borderRadius: 'var(--radius-md)', overflow: 'hidden' },
  });
  modelsList.forEach((m, i) => table.appendChild(renderModelRow(provider, m, container, i)));
  container.appendChild(table);
}

function renderModelRow(provider, m, container, idx) {
  const defaultBadge = m.is_default
    ? h('span', { className: 'badge badge--success badge--sm ml-2' }, t('prov.default'))
    : null;
  const visionBadge = m.supports_vision
    ? h('span', { className: 'badge badge--accent badge--sm ml-2', title: t('models.visionTip') }, t('models.vision'))
    : null;
  const reasoningBadge = m.supports_reasoning
    ? h('span', { className: 'badge badge--accent badge--sm ml-2', title: t('models.reasoningTip') }, t('models.reasoning'))
    : null;
  const disabled = m.is_enabled === false;
  const disabledBadge = disabled
    ? h('span', { className: 'badge badge--danger badge--sm ml-2', title: t('models.disabledTip') }, t('common.disabled'))
    : null;

  return h('div', {
    className: 'flex items-center justify-between',
    style: {
      flexWrap: 'wrap',
      gap: '0.5rem',
      padding: '0.5rem 0.75rem',
      ...(idx > 0 ? { borderTop: '1px solid var(--border)' } : {}),
      ...(disabled ? { opacity: '0.6' } : {}),
    },
  }, [
    h('div', { className: 'flex items-center gap-2' }, [
      h('span', { className: 'text-sm font-medium' }, m.model_id),
      defaultBadge,
      visionBadge,
      reasoningBadge,
      disabledBadge,
      m.display_name && m.display_name !== m.model_id
        ? h('span', { className: 'text-xs text-secondary' }, `(${m.display_name})`)
        : null,
      modelSpecLine(m),
    ].filter(Boolean)),
    ...(state.isAdmin() ? [h('div', { className: 'flex items-center gap-1' }, [
      !m.is_default ? h('button', {
        className: 'btn btn--sm',
        title: t('prov.setDefault'),
        'aria-label': t('models.setDefaultNamed', { id: m.model_id }),
        onClick: async () => {
          try {
            await post(`/admin/api/providers/${provider.id}/models/${m.id}/default`);
            toast.success(t('models.defaultUpdated'));
            loadModels(provider, container);
          } catch (err) {
            toast.error(humanizeError(err.message));
          }
        },
      }, [h('span', { innerHTML: icon('check') })]) : null,
      h('button', {
        className: 'btn btn--sm',
        title: disabled ? t('models.enableTip') : t('models.disableTip'),
        'aria-label': t(disabled ? 'models.enableNamed' : 'models.disableNamed', { id: m.model_id }),
        onClick: async () => {
          try {
            await put(`/admin/api/providers/${provider.id}/models/${m.id}`, { is_enabled: disabled });
            toast.success(disabled ? t('models.enabled') : t('models.disabled'));
            loadModels(provider, container);
          } catch (err) {
            toast.error(humanizeError(err.message));
          }
        },
      }, [h('span', { innerHTML: icon(disabled ? 'eye' : 'eye-off') })]),
      h('button', {
        className: 'btn btn--sm',
        title: t('models.edit'),
        'aria-label': t('models.editNamed', { id: m.model_id }),
        onClick: () => showEditModelModal(provider, m, container),
      }, [h('span', { innerHTML: icon('edit') })]),
      h('button', {
        className: 'btn btn--danger btn--sm',
        title: t('models.delete'),
        'aria-label': t('models.deleteNamed', { id: m.model_id }),
        onClick: () => modal.confirmDanger(
          t('models.delete'),
          t('models.deleteBody', { id: m.model_id }),
          async () => {
            try {
              await del(`/admin/api/providers/${provider.id}/models/${m.id}`);
              toast.success(t('models.deleted'));
              loadModels(provider, container);
            } catch (err) {
              toast.error(humanizeError(err.message));
            }
          },
        ),
      }, [h('span', { innerHTML: icon('trash') })]),
    ].filter(Boolean))] : []),
  ]);
}

// formatTokens renders a token count compactly: 128000 -> "128K". The picker
// lists dozens of models and the exact digits carry no decision-making weight,
// only the order of magnitude does.
function formatTokens(n) {
  if (!n || n <= 0) return '';
  return formatCompact(n);
}

// formatPrice renders USD per million tokens, dropping trailing zeros so
// $3.00 reads as $3 and $0.15 keeps its cents.
function formatPrice(v) {
  if (!v || v <= 0) return '';
  return formatUsdPrice(v);
}

// modelSpecLine renders the numbers a sync recorded: the context window, the
// per-response output cap, and the price. Rendered as one quiet line rather
// than columns because it is reference material.
function modelSpecLine(m) {
  const parts = [];
  if (m.context_tokens > 0) parts.push(t('models.ctx', { n: formatTokens(m.context_tokens) }));
  if (m.max_output_tokens > 0) parts.push(t('models.out', { n: formatTokens(m.max_output_tokens) }));
  const inP = formatPrice(m.cost_input);
  const outP = formatPrice(m.cost_output);
  if (inP && outP) parts.push(t('models.price', { input: inP, output: outP }));
  if (!parts.length) return null;
  return h('span', {
    className: 'text-xs text-secondary ml-2',
    title: t('models.specTip'),
  }, parts.join(' \u00b7 '));
}

// section groups dialog fields under a quiet title; consecutive sections are split
// by a hairline. A null title gives an untitled block (the dialog's opening fields).
function section(title, children) {
  return h('div', { className: 'form-section' }, [
    title ? h('div', { className: 'form-section__title' }, title) : null,
    ...children,
  ]);
}

// toggleField is one switch on a toggle grid; tip is the longer explanation, shown on hover.
function toggleField(check, label, tip) {
  return h('label', { className: 'toggle', title: tip }, [check, label]);
}

// limitsRow puts the context window and the output cap side by side. Their
// explanations ride on hover so the row stays two clean fields.
function limitsRow(f) {
  f.contextTokensInput.title = t('models.contextHint');
  f.maxOutputTokensInput.title = t('models.maxOutputHint');
  return h('div', { className: 'form-row' }, [
    formGroup(t('models.context'), f.contextTokensInput),
    formGroup(t('models.maxOutput'), f.maxOutputTokensInput),
  ]);
}

// capabilitiesSection lays the capability switches out on one grid. extra goes after
// the grid (the edit dialog's probe status line).
function capabilitiesSection(f, extra = null) {
  return section(t('models.sectionCapabilities'), [
    h('div', { className: 'toggle-grid' }, [
      toggleField(f.supportsStreamingCheck, t('models.capStreaming')),
      toggleField(f.supportsToolsCheck, t('models.capTools')),
      toggleField(f.supportsVisionCheck, t('models.vision'), t('models.visionHint')),
      toggleField(f.supportsReasoningCheck, t('models.reasoning'), t('models.reasoningTip')),
    ]),
    extra,
  ]);
}

// pricingFields renders the four price inputs as a collapsed section.
//
// Edit-only, deliberately. These numbers come from the provider's listing, so on
// a brand-new hand-added model there is nothing to correct yet and the first
// sync fills in what the provider reports - offering the inputs there would just
// be four fields that silently get replaced.
function pricingFields(f, overridden = []) {
  const marked = new Set(overridden);
  const label = (text, key) => marked.has(key)
    ? [text, h('span', {
        className: 'badge ml-2',
        style: { fontSize: '0.6rem' },
        title: t('models.yoursTip'),
      }, t('models.yours'))]
    : text;
  f.costCacheReadInput.title = t('models.cacheReadHint');
  f.costCacheWriteInput.title = t('models.cacheWriteHint');
  return h('details', { className: 'form-section' }, [
    h('summary', { className: 'form-section__title' }, t('models.sectionPricing')),
    h('p', { className: 'form-hint' }, t('models.pricingHint')),
    h('div', { className: 'form-row' }, [
      formGroup(label(t('models.inputPrice'), 'cost_input'), f.costInputInput),
      formGroup(label(t('models.outputPrice'), 'cost_output'), f.costOutputInput),
    ]),
    h('div', { className: 'form-row' }, [
      formGroup(label(t('models.cacheReadPrice'), 'cost_cache_read'), f.costCacheReadInput),
      formGroup(label(t('models.cacheWritePrice'), 'cost_cache_write'), f.costCacheWriteInput),
    ]),
  ]);
}

// pricingPayload is the half of the request body that pricingFields owns, plus the
// reasoning switch, which a sync also fills and an edit records as a correction.
function pricingPayload(f) {
  return {
    cost_input: parseFloat(f.costInputInput.value) || 0,
    cost_output: parseFloat(f.costOutputInput.value) || 0,
    cost_cache_read: parseFloat(f.costCacheReadInput.value) || 0,
    cost_cache_write: parseFloat(f.costCacheWriteInput.value) || 0,
    supports_reasoning: f.supportsReasoningCheck.checked,
  };
}

function modelFormFields(initial = {}) {
  const modelIdInput = h('input', { className: 'input', value: initial.model_id || '', placeholder: t('models.idPlaceholder') });
  const displayNameInput = h('input', { className: 'input', value: initial.display_name || '', placeholder: t('models.namePlaceholder') });
  const contextTokensInput = h('input', { className: 'input', type: 'number', value: String(initial.context_tokens || 4096), min: '1' });
  // Separate from the context window on purpose: they are different numbers and
  // conflating them is exactly what the old single max_tokens column did. 0
  // means "unknown", which a sync fills in when the provider reports it.
  const maxOutputTokensInput = h('input', { className: 'input', type: 'number', value: String(initial.max_output_tokens || 0), min: '0' });
  // Prices are USD per MILLION tokens, matching how every vendor quotes them.
  // step allows the sub-cent figures cheap models are actually priced at.
  const priceInput = (v) => h('input', {
    className: 'input', type: 'number', step: '0.0001', min: '0', value: String(v || 0),
  });
  const costInputInput = priceInput(initial.cost_input);
  const costOutputInput = priceInput(initial.cost_output);
  const costCacheReadInput = priceInput(initial.cost_cache_read);
  const costCacheWriteInput = priceInput(initial.cost_cache_write);
  const supportsReasoningCheck = h('input', { type: 'checkbox', checked: initial.supports_reasoning ?? false });
  const supportsStreamingCheck = h('input', { type: 'checkbox', checked: initial.supports_streaming ?? true });
  const supportsToolsCheck = h('input', { type: 'checkbox', checked: initial.supports_tools ?? true });
  const supportsVisionCheck = h('input', { type: 'checkbox', checked: !!initial.supports_vision });
  const isDefaultCheck = h('input', { type: 'checkbox', checked: !!initial.is_default });

  return {
    modelIdInput, displayNameInput, contextTokensInput, maxOutputTokensInput,
    costInputInput, costOutputInput, costCacheReadInput, costCacheWriteInput,
    supportsStreamingCheck, supportsToolsCheck, supportsVisionCheck, supportsReasoningCheck, isDefaultCheck,
  };
}

function showAddModelModal(provider, modelsContainer) {
  const f = modelFormFields();
  const content = h('div', {}, [
    section(null, [
      formGroup(t('models.modelId'), f.modelIdInput, { hint: t('models.modelIdHint') }),
      formGroup(t('users.displayName'), f.displayNameInput),
      limitsRow(f),
    ]),
    capabilitiesSection(f),
    section(null, [toggleField(f.isDefaultCheck, t('models.setAsDefault'))]),
  ]);

  modal.show(t('models.add'), content, [
    modal.cancelBtn(),
    {
      label: t('models.add'),
      className: 'btn btn--primary',
      onClick: async () => {
        const modelId = f.modelIdInput.value.trim();
        if (!modelId) { toast.error(t('models.idRequired')); return false; }
        try {
          await post(`/admin/api/providers/${provider.id}/models`, {
            model_id: modelId,
            display_name: f.displayNameInput.value.trim() || modelId,
            context_tokens: parseInt(f.contextTokensInput.value, 10) || 4096,
            max_output_tokens: parseInt(f.maxOutputTokensInput.value, 10) || 0,
            supports_streaming: f.supportsStreamingCheck.checked,
            supports_tools: f.supportsToolsCheck.checked,
            supports_vision: f.supportsVisionCheck.checked,
            supports_reasoning: f.supportsReasoningCheck.checked,
            is_default: f.isDefaultCheck.checked,
          });
          toast.success(t('models.added'));
          loadModels(provider, modelsContainer);
        } catch (err) {
          toast.error(humanizeError(err.message));
          return false;
        }
      },
    },
  ]);
}

// THINKING_STAGES mirrors models.ThinkingStages on the server, in pipeline order.
const THINKING_STAGES = [
  ['expand', 'models.thinkExpand'],
  ['plan', 'models.thinkPlan'],
  ['build', 'models.thinkBuild'],
  ['live', 'models.thinkLive'],
  ['review', 'models.thinkReview'],
];

// Per-stage reasoning switches for a reasoning model. A stage the model leaves out
// follows the engine default the server reports, so every box shows the real state.
function thinkingFields(model) {
  const checks = {};
  for (const [stage] of THINKING_STAGES) {
    const set = model.thinking && stage in model.thinking;
    checks[stage] = h('input', {
      type: 'checkbox',
      checked: set ? !!model.thinking[stage] : !!model.thinking_defaults?.[stage],
    });
  }
  const group = section(t('models.thinkingLabel'), [
    h('div', { className: 'toggle-grid' }, THINKING_STAGES.map(([stage, key]) => toggleField(checks[stage], t(key)))),
    h('p', { className: 'form-hint' }, t('models.thinkingHint')),
  ]);
  const value = () => Object.fromEntries(THINKING_STAGES.map(([stage]) => [stage, checks[stage].checked]));
  return { group, value };
}

function showEditModelModal(provider, model, modelsContainer) {
  const f = modelFormFields(model);
  // The stage switches only mean something for a reasoning model, so they follow the
  // reasoning toggle live: ticking it on a model the catalog did not flag shows them.
  const thinking = thinkingFields(model);
  const syncThinking = () => { thinking.group.hidden = !f.supportsReasoningCheck.checked; };
  f.supportsReasoningCheck.addEventListener('change', syncThinking);
  syncThinking();
  // Reasoning and tool use are probed live when the listing does not report them:
  // on open for a model not yet checked, and on "check again".
  const probeLine = provider.probes_caps ? h('p', { className: 'form-hint' }) : null;
  const content = h('div', {}, [
    section(null, [
      h('div', { className: 'form-group' }, [
        h('label', {}, t('models.modelId')),
        h('div', { className: 'form-readonly', title: t('models.idImmutable') }, model.model_id),
      ]),
      formGroup(t('users.displayName'), f.displayNameInput),
      limitsRow(f),
    ]),
    capabilitiesSection(f, probeLine),
    thinking.group,
    pricingFields(f, model.overridden),
  ]);

  const editPayload = () => ({
    display_name: f.displayNameInput.value.trim() || model.model_id,
    context_tokens: parseInt(f.contextTokensInput.value, 10) || 4096,
    max_output_tokens: parseInt(f.maxOutputTokensInput.value, 10) || 0,
    supports_streaming: f.supportsStreamingCheck.checked,
    supports_tools: f.supportsToolsCheck.checked,
    supports_vision: f.supportsVisionCheck.checked,
    ...pricingPayload(f),
  });
  const initialPayload = editPayload();
  const initialThinking = JSON.stringify(thinking.value());

  // runProbe asks the server to check the model and folds the answer into the
  // switches. A switch the admin already moved keeps their value, and the baseline
  // moves with the probe so an untouched probed value is not sent as a correction.
  const runProbe = async (force) => {
    if (!probeLine) return;
    clear(probeLine);
    probeLine.textContent = t('models.checking');
    try {
      const fresh = await post(`/admin/api/providers/${provider.id}/models/${model.id}/probe${force ? '?force=1' : ''}`);
      for (const [key, check] of [['supports_reasoning', f.supportsReasoningCheck], ['supports_tools', f.supportsToolsCheck]]) {
        if (check.checked === initialPayload[key]) {
          check.checked = !!fresh[key];
          initialPayload[key] = check.checked;
        }
      }
      syncThinking();
      showProbeLine(fresh.probe_pending?.length ? t('models.checkUnsure') : t('models.checked'));
    } catch (err) {
      showProbeLine(humanizeError(err.message));
    }
  };
  const showProbeLine = (text) => {
    clear(probeLine);
    probeLine.append(text, ' ', h('a', {
      href: '#',
      onClick: (e) => { e.preventDefault(); runProbe(true); },
    }, t('models.checkAgain')));
  };
  if (probeLine) {
    if (model.probe_pending?.length) runProbe(false);
    else showProbeLine(t('models.checked'));
  }

  const buttons = [
    modal.cancelBtn(),
    {
      label: t('common.save'),
      className: 'btn btn--primary',
      onClick: async () => {
        try {
          // Send only what the admin changed. Every field the server receives is
          // recorded as a correction that a model sync will not overwrite, so
          // submitting untouched fields would freeze them all against the catalog.
          const now = editPayload();
          const changed = {};
          for (const key of Object.keys(now)) {
            if (now[key] !== initialPayload[key]) changed[key] = now[key];
          }
          // Thinking is not a catalog correction, so it goes as one whole map
          // whenever any stage changed.
          if (f.supportsReasoningCheck.checked && JSON.stringify(thinking.value()) !== initialThinking) {
            changed.thinking = thinking.value();
          }
          if (Object.keys(changed).length === 0) {
            toast.info(t('models.nothingChanged'));
            return;
          }
          await put(`/admin/api/providers/${provider.id}/models/${model.id}`, changed);
          toast.success(t('models.updated'));
          loadModels(provider, modelsContainer);
        } catch (err) {
          toast.error(humanizeError(err.message));
          return false;
        }
      },
    },
  ];

  // Only offered once there is something to reset. Drops the recorded
  // corrections so the next model sync hands the row back to the provider values.
  if (model.overridden?.length) {
    buttons.splice(1, 0, {
      label: t('models.reset'),
      className: 'btn',
      onClick: async () => {
        try {
          await put(`/admin/api/providers/${provider.id}/models/${model.id}`, { reset_overrides: true });
          toast.success(t('models.resetDone'));
          loadModels(provider, modelsContainer);
        } catch (err) {
          toast.error(humanizeError(err.message));
          return false;
        }
      },
    });
  }

  modal.show(t('models.edit'), content, buttons);
}
