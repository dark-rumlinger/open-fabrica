/**
 * Shared model picker, provider + model select pair.
 */

import { h } from '../core/dom.js';

/**
 * Create a provider/model picker from a provider catalog.
 * @param {Array} providers - Provider catalog from /admin/api/providers/catalog.
 * @param {Object} [opts]
 * @param {number} [opts.currentModelId] - Pre-select the provider/model matching this ID (use when editing an existing selection).
 * @param {number} [opts.preferredModelId] - Soft hint (e.g. last-used model). Honored only when no system default is set.
 * @param {boolean} [opts.disabled] - Disable both selects.
 * @returns {{ providerSelect: HTMLSelectElement, modelSelect: HTMLSelectElement, getSelectedModel: () => Object|null }}
 */
export function createModelPicker(providers, opts = {}) {
  const { currentModelId = null, preferredModelId = null, disabled = false } = opts;

  // The admin-set system default outranks the last-used preference hint.
  const hasSystemDefault = providers.some(p => (p.models || []).some(m => m.is_default));
  const effectivePreferredId = (!currentModelId && !hasSystemDefault) ? preferredModelId : null;

  let selectedProvider = null;
  let selectedModel = null;

  const providerSelect = h('select', { className: 'input', disabled });
  const modelSelect = h('select', { className: 'input', disabled });

  providers.forEach((p) => {
    providerSelect.appendChild(h('option', { value: String(p.id) }, p.name));
  });

  function updateModels() {
    modelSelect.innerHTML = '';
    selectedModel = null;
    if (!selectedProvider || !selectedProvider.models) return;

    selectedProvider.models.forEach((m) => {
      const opt = h('option', { value: String(m.id) }, m.display_name);
      let shouldSelect;
      if (currentModelId) {
        shouldSelect = m.id === currentModelId;
      } else if (hasSystemDefault) {
        shouldSelect = m.is_default;
      } else if (effectivePreferredId) {
        shouldSelect = m.id === effectivePreferredId;
      } else {
        shouldSelect = false;
      }
      if (shouldSelect) {
        opt.selected = true;
        selectedModel = m;
      }
      modelSelect.appendChild(opt);
    });

    if (!selectedModel && selectedProvider.models.length > 0) {
      selectedModel = selectedProvider.models[0];
      modelSelect.options[0].selected = true;
    }
  }

  // Find initial provider. Priority order:
  //   1. Provider that owns currentModelId (editing an existing selection).
  //   2. Provider that owns the system default model (is_default=true).
  //   3. Provider that owns the preferred (e.g. last-used) model, only when no system default exists.
  //   4. First provider in the list.
  if (currentModelId) {
    for (const p of providers) {
      if (p.models && p.models.some(m => m.id === currentModelId)) {
        selectedProvider = p;
        break;
      }
    }
  }
  if (!selectedProvider) {
    for (const p of providers) {
      if (p.models && p.models.some(m => m.is_default)) {
        selectedProvider = p;
        break;
      }
    }
  }
  if (!selectedProvider && effectivePreferredId) {
    for (const p of providers) {
      if (p.models && p.models.some(m => m.id === effectivePreferredId)) {
        selectedProvider = p;
        break;
      }
    }
  }
  if (!selectedProvider && providers.length > 0) {
    selectedProvider = providers[0];
  }
  if (selectedProvider) {
    providerSelect.value = String(selectedProvider.id);
  }

  providerSelect.addEventListener('change', () => {
    const id = parseInt(providerSelect.value, 10);
    selectedProvider = providers.find(p => p.id === id) || null;
    updateModels();
  });

  modelSelect.addEventListener('change', () => {
    if (!selectedProvider) return;
    const mid = parseInt(modelSelect.value, 10);
    selectedModel = selectedProvider.models.find(m => m.id === mid) || null;
  });

  updateModels();

  return {
    providerSelect,
    modelSelect,
    getSelectedModel() { return selectedModel; },
  };
}
