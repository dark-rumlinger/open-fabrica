import { h, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { get, post, put, del } from '../../core/http.js';
import { icon } from '../../ui/icon.js';
import * as toast from '../../ui/toast.js';
import * as modal from '../../ui/modal.js';
import { createScope } from '../../core/lifecycle.js';
import { emptyState, errorState, formGroup, clickable, watchToolReload, copyText, publicOrigin } from '../../ui/helpers.js';
import * as state from '../../core/state.js';
import { safeJsonParse } from '../../core/utils.js';
import { pageHeader } from '../../ui/card.js';
import { humanizeError } from '../../ui/errors.js';
import { runAction } from '../../ui/actions.js';
import { createFormModal } from '../../ui/form-modal.js';

const ALL_METHODS = ['GET', 'POST', 'PUT', 'DELETE', 'PATCH'];

// Where visitors reach this project, so Copy URL hands out an address that
// works from outside. Set per render; only one panel is mounted at a time.
let publicBase = '';

export async function renderProjectEndpoints(container, projectId, project) {
  clear(container);
  const scope = createScope();
  publicBase = publicOrigin(project, state.get('systemStatus'));

  const header = pageHeader({
    title: t('tabs.apis'),
  });
  const body = h('div', { className: 'context-panel__body' });
  container.appendChild(header);
  container.appendChild(body);

  const reload = () => loadEndpoints(body, projectId);

  watchToolReload(scope, projectId,
    { tools: ['manage_api', 'manage_auth', 'manage_relay', 'manage_tick', 'manage_authoritative', 'manage_matchmaking', 'manage_signaling', 'manage_realtime', 'manage_sse', 'manage_llm_proxy',
      'manage_function', 'manage_agent', 'manage_documents', 'manage_export_endpoint', 'manage_import_endpoint',
      'manage_feeds', 'manage_push', 'manage_2fa', 'manage_uploads'] },
    () => reload());

  await reload();
  return () => scope.dispose();
}

async function loadEndpoints(container, projectId) {
  let apiEndpoints, authEndpoints;
  try {
    [apiEndpoints, authEndpoints] = await Promise.all([
      get(`/admin/api/projects/${projectId}/endpoints`),
      get(`/admin/api/projects/${projectId}/auth-endpoints`),
    ]);
  } catch (err) {
    clear(container);
    container.appendChild(errorState({
      message: t('api.loadFailed', { error: humanizeError(err.message) }),
      retry: () => loadEndpoints(container, projectId),
    }));
    return;
  }
  renderEndpoints(container, apiEndpoints || [], authEndpoints || [], projectId);
}

function renderEndpoints(container, apiEndpoints, authEndpoints, projectId) {
  clear(container);
  const reload = () => loadEndpoints(container, projectId);
  const canWrite = state.isAdmin();

  // Data APIs are the one kind this page can edit or delete. Every other kind
  // (AI endpoints, WebSockets, uploads, exports, functions ...) is created and
  // changed by the builder, so it is listed read-only. Rows from an older server
  // carry no kind and are treated as data APIs.
  const dataApis = apiEndpoints.filter((e) => !e.kind || e.kind === 'api');
  const otherEndpoints = apiEndpoints.filter((e) => e.kind && e.kind !== 'api');

  // Nothing yet: the empty state carries the action, so there is no lone
  // button hanging above an otherwise blank panel.
  if (apiEndpoints.length === 0 && authEndpoints.length === 0) {
    container.appendChild(emptyState({
      iconName: 'zap',
      title: t('api.empty.title'),
      description: t('api.empty.description'),
      action: canWrite ? { label: t('api.create'), onClick: () => showCreateModal(projectId, reload) } : null,
    }));
    return;
  }

  if (canWrite) {
    container.appendChild(h('div', { className: 'flex items-center justify-end mb-3' }, [
      h('button', {
        className: 'btn btn--primary btn--sm',
        onClick: () => showCreateModal(projectId, reload),
      }, [h('span', { innerHTML: icon('plus') }), ' ' + t('api.create')]),
    ]));
  }

  if (dataApis.length > 0) {
    container.appendChild(sectionHeader(t('api.dataApis')));
    for (const ep of dataApis) container.appendChild(renderApiCard(ep, projectId, reload));
  }

  if (authEndpoints.length > 0) {
    container.appendChild(sectionHeader(t('api.loginApis'), dataApis.length > 0));
    for (const ep of authEndpoints) container.appendChild(renderAuthCard(ep, projectId, reload));
  }

  if (otherEndpoints.length > 0) {
    container.appendChild(sectionHeader(t('api.other'), dataApis.length > 0 || authEndpoints.length > 0));
    container.appendChild(h('p', { className: 'form-hint mb-2', style: { padding: '0 4px' } },
      t('api.otherHint')));
    for (const ep of otherEndpoints) container.appendChild(renderOtherCard(ep));
  }
}

// The icon for each read-only kind; anything new falls back to the lightning bolt.
const KIND_ICONS = {
  llm: 'zap', agent: 'zap', function: 'code', ws: 'zap', stream: 'zap', upload: 'upload',
  document: 'file-text', export: 'download', import: 'upload', query: 'search',
  feed: 'rss', push: 'bell', totp: 'shield', magic_link: 'mail',
};

/**
 * The public URL (path only) a read-only endpoint is reached at. The shapes differ
 * by kind: feeds live under /feed/, exports and imports add a suffix, a two-factor
 * or email-link endpoint is a set of routes on its login API, and the rest sit
 * directly under /api/.
 */
export function endpointUrl(ep) {
  switch (ep.kind) {
    case 'feed': return `/feed/${ep.path}`;
    case 'export': return `/api/${ep.path}/export`;
    case 'import': return `/api/${ep.path}/import`;
    case 'push': return `/api/${ep.path}/push/subscribe`;
    case 'totp': return `/api/${ep.path}/totp/...`;
    case 'magic_link': return `/api/${ep.path}/magic/...`;
    default: return `/api/${ep.path}`;
  }
}

function renderOtherCard(ep) {
  const url = endpointUrl(ep);
  const methods = safeJsonParse(ep.methods, []);
  const copyable = !url.endsWith('...');
  return h('div', { className: 'card mb-3' }, [
    h('div', { className: 'card__header' }, [
      h('div', { className: 'flex items-center gap-2' }, [
        h('span', { innerHTML: icon(KIND_ICONS[ep.kind] || 'zap') }),
        h('code', { style: { fontSize: 'var(--text-sm)' } }, url),
        ...(copyable ? [copyUrlBtn(null, url)] : []),
      ]),
    ]),
    h('div', { style: { padding: '0 12px 12px' } }, [
      h('div', { className: 'flex items-center gap-2 mb-2' }, [
        h('span', { className: 'badge badge--info' }, ep.table_name),
        ...methods.map((m) => h('span', { className: 'badge' }, m)),
      ]),
      ...(ep.detail ? [h('div', { className: 'text-sm text-secondary mb-2' }, ep.detail)] : []),
      h('div', { className: 'flex items-center gap-3 text-sm text-secondary' }, [
        ep.requires_auth
          ? h('span', { className: 'flex items-center gap-1' }, [h('span', { innerHTML: icon('lock') }), t('api.loginRequired')])
          : h('span', { className: 'flex items-center gap-1' }, [h('span', { innerHTML: icon('globe') }), t('api.public')]),
        ...(ep.rate_limit ? [h('span', {}, t('api.perMin', { n: ep.rate_limit }))] : []),
      ]),
      llmBudgetLine(ep),
    ]),
  ]);
}

function sectionHeader(title, withTopMargin = false) {
  return h('h4', {
    className: 'text-sm text-secondary mb-2',
    style: { padding: '0 4px', marginTop: withTopMargin ? '16px' : '0' },
  }, title);
}

function copyUrlBtn(path, urlPath) {
  return h('button', {
    className: 'btn btn--ghost btn--sm',
    title: t('api.copyUrl'),
    'aria-label': t('api.copyUrl'),
    onClick: (e) => {
      e.stopPropagation();
      copyText(`${publicBase}${urlPath || `/api/${path}`}`, t('helpers.copied'));
    },
  }, [h('span', { innerHTML: icon('copy') })]);
}

function renderApiCard(ep, projectId, reload) {
  const methods = safeJsonParse(ep.methods, []);
  const card = h('div', { className: 'card mb-3' }, [
    h('div', { className: 'card__header' }, [
      h('div', { className: 'flex items-center gap-2' }, [
        h('span', { innerHTML: icon('zap') }),
        h('code', { style: { fontSize: 'var(--text-sm)' } }, `/api/${ep.path}`),
        copyUrlBtn(ep.path),
      ]),
      ...(state.isAdmin() ? [h('div', { className: 'flex items-center gap-1' }, [
        h('button', {
          className: 'btn btn--ghost btn--sm',
          title: t('api.edit'),
          'aria-label': t('api.edit'),
          onClick: (e) => { e.stopPropagation(); showEditModal(ep, projectId, reload); },
        }, [h('span', { innerHTML: icon('edit') })]),
        h('button', {
          className: 'btn btn--ghost btn--sm',
          title: t('api.delete'),
          'aria-label': t('api.delete'),
          onClick: (e) => {
            e.stopPropagation();
            confirmDelete(t('api.deleteBody', { path: `/api/${ep.path}` }),
              `/admin/api/projects/${projectId}/endpoints/${ep.id}`, t('api.deleted'), reload);
          },
        }, [h('span', { innerHTML: icon('trash') })]),
      ])] : []),
    ]),
    h('div', { style: { padding: '0 12px 12px' } }, [
      h('div', { className: 'flex items-center gap-2 mb-2' }, [
        h('span', { className: 'text-sm text-secondary' }, t('api.table')),
        h('span', { className: 'badge badge--info' }, ep.table_name),
      ]),
      h('div', { className: 'flex items-center gap-2 mb-2' }, methods.map(m => h('span', { className: 'badge' }, m))),
      h('div', { className: 'flex items-center gap-3 text-sm text-secondary' }, [
        ep.requires_auth
          ? h('span', { className: 'flex items-center gap-1' }, [h('span', { innerHTML: icon('lock') }), t('api.loginRequired')])
          : h('span', { className: 'flex items-center gap-1' }, [h('span', { innerHTML: icon('globe') }), t('api.public')]),
        ...(ep.rate_limit ? [h('span', {}, t('api.perMin', { n: ep.rate_limit }))] : []),
      ]),
      llmBudgetLine(ep),
    ]),
  ]);
  if (state.isAdmin()) clickable(card, () => showEditModal(ep, projectId, reload));
  return card;
}

// llmBudgetLine renders the per-day token budget + today's usage for LLM
// proxy endpoints. Returns null for non-LLM rows so renderApiCard stays
// untouched for API endpoints. Surfaces the same numbers manage_llm_proxy's
// list action returns; lets admins spot a runaway endpoint at a glance.
function llmBudgetLine(ep) {
  if (ep.table_name !== 'LLM') return null;
  const used = ep.daily_tokens_used || 0;
  const budget = ep.daily_token_budget || 0;
  const today = new Date().toISOString().slice(0, 10);
  // Counter only meaningful when today's window is the one on the row.
  // Anything else is yesterday's stale total waiting for the next request
  // to roll the window forward.
  const stale = ep.day_window_start && ep.day_window_start !== today;
  const usedLabel = stale ? t('api.stale', { used }) : `${used}`;
  const budgetLabel = budget > 0 ? t('api.budget', { used: usedLabel, budget }) : t('api.noCap', { used: usedLabel });
  return h('div', { className: 'flex items-center gap-2 text-sm text-secondary mt-1' }, [
    h('span', { innerHTML: icon('bar-chart') }),
    h('span', {}, budgetLabel),
  ]);
}

function renderAuthCard(ep, projectId, reload) {
  const routes = ep.routes || [];
  return h('div', { className: 'card mb-3' }, [
    h('div', { className: 'card__header' }, [
      h('div', { className: 'flex items-center gap-2' }, [
        h('span', { innerHTML: icon('key') }),
        h('code', { style: { fontSize: 'var(--text-sm)' } }, `/api/${ep.path}`),
        copyUrlBtn(ep.path),
        h('span', { className: 'badge badge--warning' }, t('sp.auth')),
      ]),
      ...(state.isAdmin() ? [h('button', {
        className: 'btn btn--ghost btn--sm',
        title: t('api.deleteLogin'),
        'aria-label': t('api.deleteLogin'),
        onClick: (e) => {
          e.stopPropagation();
          confirmDelete(t('api.deleteLoginBody', { path: `/api/${ep.path}` }),
            `/admin/api/projects/${projectId}/auth-endpoints/${ep.id}`, t('api.loginDeleted'), reload);
        },
      }, [h('span', { innerHTML: icon('trash') })])] : []),
    ]),
    h('div', { style: { padding: '0 12px 12px' } }, [
      h('div', { className: 'flex items-center gap-2 mb-2' }, [
        h('span', { className: 'text-sm text-secondary' }, t('api.table')),
        h('span', { className: 'badge badge--info' }, ep.table_name),
      ]),
      h('div', { className: 'mb-2' },
        routes.map(r => h('div', { className: 'text-sm', style: { fontFamily: 'var(--font-mono)', padding: '2px 0' } }, r))),
      h('div', { className: 'flex items-center gap-3 text-sm text-secondary' }, [
        h('span', {}, t('api.loginColumn', { column: ep.username_column })),
      ]),
    ]),
  ]);
}

function confirmDelete(message, url, successMsg, reload) {
  modal.confirmDanger(t('common.delete'), message,
    () => runAction(() => del(url), { success: successMsg, onDone: reload }));
}

function methodToggleRow(initialSelection) {
  const checks = ALL_METHODS.map(m => h('input', { type: 'checkbox', checked: initialSelection.includes(m) }));
  const labels = ALL_METHODS.map((m, i) =>
    h('label', { className: 'toggle', style: { marginInlineEnd: '8px' } }, [checks[i], m]));
  return {
    row: h('div', { className: 'flex items-center', style: { flexWrap: 'wrap' } }, labels),
    selected: () => ALL_METHODS.filter((_, i) => checks[i].checked),
  };
}

function showEditModal(ep, projectId, reload) {
  const methods = methodToggleRow(safeJsonParse(ep.methods, []));
  const authCheck = h('input', { type: 'checkbox', checked: ep.requires_auth });
  const publicReadCheck = h('input', { type: 'checkbox', checked: ep.public_read });
  const roleInput = h('input', { className: 'input', value: ep.required_role || '', placeholder: t('api.rolePlaceholder') });
  const rateInput = h('input', { className: 'input', type: 'number', value: ep.rate_limit, min: '1' });

  createFormModal({
    title: t('api.editTitle', { path: `/api/${ep.path}` }),
    fields: [
      readOnlyField('URL', `/api/${ep.path}`, true),
      readOnlyField(t('api.dataTable'), ep.table_name),
      h('div', { className: 'form-group mb-3' }, [
        h('label', { className: 'text-sm text-secondary mb-1' }, t('api.allowedActions')),
        methods.row,
        h('p', { className: 'form-hint' }, t('api.methodsHintFull')),
      ]),
      h('div', { className: 'form-group mb-3' }, [
        h('label', { className: 'toggle' }, [authCheck, t('api.requireLogin')]),
        h('p', { className: 'form-hint' }, t('api.requireLoginHint')),
      ]),
      h('div', { className: 'form-group mb-3' }, [
        h('label', { className: 'toggle' }, [publicReadCheck, t('api.publicRead')]),
        h('p', { className: 'form-hint' }, t('api.publicReadHintEdit')),
      ]),
      formGroup(t('api.requiredRole'), roleInput, {
        hint: t('api.requiredRoleHint'),
      }),
      formGroup(t('api.rateLimit'), rateInput, {
        hint: t('api.rateLimitHint'),
      }),
    ],
    validate: () => {
      if (methods.selected().length === 0) return t('api.needMethod');
      const rate = parseInt(rateInput.value, 10);
      if (!Number.isInteger(rate) || rate < 1) return t('api.rateInvalid');
      return null;
    },
    submit: () => put(`/admin/api/projects/${projectId}/endpoints/${ep.id}`, {
      methods: methods.selected(),
      requires_auth: authCheck.checked,
      public_read: publicReadCheck.checked,
      required_role: roleInput.value.trim(),
      rate_limit: parseInt(rateInput.value, 10),
    }),
    success: t('api.updated'),
    onDone: reload,
  });
}

function readOnlyField(label, value, mono = false) {
  return h('div', { className: 'form-group mb-3' }, [
    h('label', { className: 'text-sm text-secondary' }, label),
    h('div', {
      className: 'text-sm',
      style: { padding: '6px 0', ...(mono ? { fontFamily: 'var(--font-mono)' } : {}) },
    }, value),
  ]);
}

async function showCreateModal(projectId, reload) {
  let tables = [];
  try {
    tables = await get(`/admin/api/projects/${projectId}/tables`);
  } catch (err) {
    toast.error(t('api.tablesFailed', { error: humanizeError(err.message) }));
    return;
  }
  if (tables.length === 0) {
    toast.info(t('api.noTables'));
    return;
  }

  const pathInput = h('input', { className: 'input', placeholder: t('api.pathPlaceholder') });
  const tableSelect = h('select', { className: 'input' }, [
    h('option', { value: '' }, t('api.selectTable')),
    ...tables.map(tbl => h('option', { value: tbl.table_name }, tbl.table_name)),
  ]);

  const methods = methodToggleRow(['GET', 'POST', 'PUT', 'DELETE']);
  const authCheck = h('input', { type: 'checkbox' });
  const publicReadCheck = h('input', { type: 'checkbox' });
  const ownerInput = h('input', { className: 'input', placeholder: t('api.ownerPlaceholder') });

  createFormModal({
    title: t('api.create'),
    fields: [
      formGroup(t('api.urlPath'), pathInput, { hint: t('api.urlPathHint') }),
      formGroup(t('api.dataTable'), tableSelect, { hint: t('api.tableHint') }),
      formGroup(t('api.allowedActions'), methods.row, { hint: t('api.methodsHint') }),
      h('div', { className: 'form-group' }, [
        h('label', { className: 'toggle' }, [authCheck, t('api.requireLogin')]),
        h('p', { className: 'form-hint' }, t('api.requireLoginHint')),
      ]),
      h('div', { className: 'form-group' }, [
        h('label', { className: 'toggle' }, [publicReadCheck, t('api.publicRead')]),
        h('p', { className: 'form-hint' }, t('api.publicReadHint')),
      ]),
      formGroup(t('api.ownerColumn'), ownerInput, {
        hint: t('api.ownerHint'),
      }),
    ],
    submitLabel: t('rc.create'),
    validate: () => {
      if (!pathInput.value.trim() || !tableSelect.value) return t('api.pathRequired');
      if (methods.selected().length === 0) return t('api.needMethod');
      return null;
    },
    submit: () => post(`/admin/api/projects/${projectId}/endpoints`, {
      path: pathInput.value.trim(),
      table_name: tableSelect.value,
      methods: methods.selected(),
      requires_auth: authCheck.checked,
      public_read: publicReadCheck.checked,
      owner_column: ownerInput.value.trim(),
    }),
    success: t('api.created'),
    onDone: reload,
  });
}
