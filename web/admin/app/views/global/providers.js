import { h, clear } from '../../core/dom.js';
import { t, intlTag } from '../../core/i18n.js';
import { get } from '../../core/http.js';
import { icon } from '../../ui/icon.js';
import * as state from '../../core/state.js';
import { defineView } from '../../core/view.js';
import { pageHeader, pageBody } from '../../ui/card.js';
import { createListView } from '../../ui/list-view.js';
import { loadModels } from './providers/models.js';
import { isCodingPlan } from '../../ui/signup-hint.js';
import { showAddProviderModal, showEditProviderModal, confirmDeleteProvider, testProvider, syncModels, syncAllProviders, setDefaultProvider, checkCapabilities } from './providers/provider-modals.js';

// kindsWithDefaultButton: the "Set as Default" affordance only makes
// sense for kinds where the resolver picks a single provider per call
// (image, stock). LLM defaults live at the model level and have their
// own UI in the Models drawer, so they're excluded here.
const kindsWithDefaultButton = new Set(['image', 'stock', 'search']);

// kindsWithModelsSection: stock providers ship a single synthetic
// "default" model that nobody needs to manage, so the drawer is hidden.
// Image keeps the section because providers can legitimately carry
// multiple models worth enabling/disabling.
const kindsWithModelsSection = new Set(['llm', 'image']);

// kindCopy keeps per-kind UI strings in one place so the page wrappers stay
// one-liners. Title is the menu+header text, emptyTitle/emptyDescription are
// the empty state, addLabel is the primary action button label. Single source
// of truth, swap a string here and every caller updates.
const kindCopy = {
  llm: {
    title: t('nav.llm'),
    emptyIcon: 'brain',
    emptyTitle: t('prov.llm.emptyTitle'),
    emptyDescription: t('prov.llm.emptyDescription'),
    addLabel: t('prov.llm.add'),
    errorMsg: t('prov.llm.loadFailed'),
  },
  image: {
    title: t('prov.image.title'),
    emptyIcon: 'sparkle',
    emptyTitle: t('prov.image.emptyTitle'),
    emptyDescription: t('prov.image.emptyDescription'),
    addLabel: t('prov.image.add'),
    errorMsg: t('prov.image.loadFailed'),
  },
  stock: {
    title: t('prov.stock.title'),
    emptyIcon: 'image',
    emptyTitle: t('prov.stock.emptyTitle'),
    emptyDescription: t('prov.stock.emptyDescription'),
    addLabel: t('prov.stock.add'),
    errorMsg: t('prov.stock.loadFailed'),
  },
  search: {
    title: t('nav.search'),
    emptyIcon: 'search',
    emptyTitle: t('prov.search.emptyTitle'),
    emptyDescription: t('prov.search.emptyDescription'),
    addLabel: t('prov.search.add'),
    errorMsg: t('prov.search.loadFailed'),
  },
};

// renderProvidersForKind drives every kind-scoped page. Fetch is filtered
// server-side via ?kind=, and the empty/header copy varies, the renderer
// itself is identical because the provider row schema is the same across
// kinds. Wrapper exports keep the router free of kind-knowledge.
function renderProvidersForKind(container, kind, scope) {
  container.replaceChildren();
  const copy = kindCopy[kind];

  const expandedModels = new Set();
  const body = pageBody();
  const list = createListView({
    scope,
    container: body,
    fetch: () => get(`/admin/api/providers?kind=${encodeURIComponent(kind)}`),
    renderItem: (p) => renderProviderCard(p, expandedModels, () => list.reload()),
    filter: true,
    empty: {
      iconName: copy.emptyIcon,
      title: copy.emptyTitle,
      description: copy.emptyDescription,
      action: state.isAdmin() ? { label: copy.addLabel, onClick: () => showAddProviderModal(kind, () => list.reload()) } : null,
    },
    errorMsg: copy.errorMsg,
  });

  const headerActions = [];
  if (state.isAdmin() && kind === 'llm') {
    headerActions.push({
      label: t('prov.syncAll'),
      iconName: 'rotate-ccw',
      kind: 'secondary',
      onClick: () => syncAllProviders(list.reload),
    });
  }
  if (state.isAdmin()) {
    headerActions.push({
      label: copy.addLabel,
      iconName: 'plus',
      kind: 'primary',
      onClick: () => showAddProviderModal(kind, list.reload),
    });
  }

  const header = pageHeader({
    title: copy.title,
    actions: headerActions,
  });

  container.appendChild(header);
  container.appendChild(body);
  list.reload();
}

export const renderLLMProviders    = defineView((ctx, container) => renderProvidersForKind(container, 'llm', ctx.scope));
export const renderImageProviders  = defineView((ctx, container) => renderProvidersForKind(container, 'image', ctx.scope));
export const renderStockProviders  = defineView((ctx, container) => renderProvidersForKind(container, 'stock', ctx.scope));
export const renderSearchProviders = defineView((ctx, container) => renderProvidersForKind(container, 'search', ctx.scope));

// providerStatus returns the {label, kind} pair for the status badge based
// on the three independent flags the backend now sends. Order of checks
// matches the admin's mental model: explicit "off" first, then "available
// without setup" (Ollama-style), then "available with key", then "needs key".
function providerStatus(p) {
  if (p.is_enabled === false) return { label: t('common.disabled'), kind: 'badge--danger' };
  if (p.is_available && !p.requires_api_key) return { label: t('prov.readyLocal'), kind: 'badge--success' };
  if (p.is_available) return { label: t('prov.ready'), kind: 'badge--success' };
  if (p.requires_api_key && !p.has_api_key) return { label: t('prov.needsKey'), kind: 'badge--warning' };
  return { label: t('prov.unavailable'), kind: 'badge--warning' };
}

// healthNote reports what the last real calls to the provider returned, so a key
// that stopped working shows here without pressing Test. Null until a call was made.
function healthNote(p) {
  const hl = p.health;
  if (!hl) return null;
  const when = (at) => (at ? new Date(at).toLocaleString(intlTag()) : '');
  if (hl.consecutive_failures > 0) {
    const streak = hl.consecutive_failures > 1 ? ' ' + t('prov.inARow', { count: hl.consecutive_failures }) : '';
    return h('p', { className: 'form-hint mt-2', style: { color: 'var(--danger)' } },
      t('prov.lastFailed', { when: when(hl.last_error_at), streak, error: hl.last_error }));
  }
  if (hl.last_ok) return h('p', { className: 'form-hint mt-2' }, t('prov.lastOk', { when: when(hl.last_ok) }));
  return null;
}

function providerIcon(p) {
  if (p.kind === 'image') return 'sparkle';
  if (p.kind === 'stock') return 'image';
  if (p.kind === 'search') return 'search';
  if (p.transport === 'anthropic') return 'brain';
  return 'zap';
}

function renderProviderCard(p, expandedModels, reload) {
  const status = providerStatus(p);
  const statusBadge = h('span', { className: `badge ${status.kind}` }, status.label);
  const showDefaultBtn = kindsWithDefaultButton.has(p.kind);
  const showModelsBtn = kindsWithModelsSection.has(p.kind);
  const defaultBadge = showDefaultBtn && p.is_default
    ? h('span', { className: 'badge badge--success' }, t('prov.default'))
    : null;
  const planBadge = isCodingPlan(p)
    ? h('span', { className: 'badge badge--warning', title: t('prov.codingPlanTip') }, t('setup.provider.badgeCoding'))
    : null;

  const modelsContainer = h('div', { className: 'mt-4' });
  const isAdmin = state.isAdmin();

  const card = h('div', { className: 'card mb-4' }, [
    h('div', { className: 'card__header' }, [
      h('div', { className: 'flex items-center gap-3' }, [
        h('span', { innerHTML: icon(providerIcon(p)) }),
        h('div', {}, [
          h('h4', { className: 'card__title' }, p.name),
          h('span', { className: 'text-xs text-secondary' }, p.vendor || p.transport),
        ]),
      ]),
      h('div', { className: 'flex items-center gap-2' }, [
        ...(planBadge ? [planBadge] : []),
        ...(defaultBadge ? [defaultBadge] : []),
        statusBadge,
      ]),
    ]),
    healthNote(p),
    h('div', { className: 'flex flex-wrap items-center gap-2 mt-4' }, [
      ...(isAdmin ? [actionBtn(t('prov.test'), 'activity', () => testProvider(p.id))] : []),
      ...(showModelsBtn ? [actionBtn(t('prov.models'), 'list', () => toggleModels(p, expandedModels, modelsContainer))] : []),
      ...(isAdmin && showModelsBtn && p.supports_sync ? [
        actionBtn(t('prov.syncModels'), 'rotate-ccw', () => syncModels(p.id, () => {
          if (expandedModels.has(p.id)) loadModels(p, modelsContainer);
        })),
      ] : []),
      ...(isAdmin && showModelsBtn && p.probes_caps ? [
        actionBtn(t('pm.checkCapabilities'), 'activity', () => checkCapabilities(p.id, () => {
          if (expandedModels.has(p.id)) loadModels(p, modelsContainer);
        })),
      ] : []),
      ...(isAdmin && showDefaultBtn && !p.is_default
        ? [actionBtn(t('prov.setDefault'), 'star', () => setDefaultProvider(p.id, reload))]
        : []),
      ...(isAdmin ? [
        actionBtn(t('common.edit'), 'edit', () => showEditProviderModal(p, reload)),
        actionBtn(t('common.delete'), 'trash', () => confirmDeleteProvider(p, reload), 'btn btn--danger btn--sm'),
      ] : []),
    ]),
    modelsContainer,
  ]);

  if (showModelsBtn && expandedModels.has(p.id)) loadModels(p, modelsContainer);
  return card;
}

// An action that takes a moment (a connection test, a model sync) disables its
// button until it settles, so a second click cannot start a duplicate.
function actionBtn(label, iconName, onClick, className = 'btn btn--sm') {
  const btn = h('button', {
    className,
    type: 'button',
    onClick: async () => {
      if (btn.disabled) return;
      btn.disabled = true;
      try {
        await onClick();
      } finally {
        btn.disabled = false;
      }
    },
  }, [
    h('span', { innerHTML: icon(iconName) }),
    label,
  ]);
  return btn;
}

function toggleModels(provider, expandedModels, container) {
  if (expandedModels.has(provider.id)) {
    expandedModels.delete(provider.id);
    clear(container);
  } else {
    expandedModels.add(provider.id);
    loadModels(provider, container);
  }
}
