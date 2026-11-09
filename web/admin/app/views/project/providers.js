import { h } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { get, post, del } from '../../core/http.js';
import { createScope } from '../../core/lifecycle.js';
import * as modal from '../../ui/modal.js';
import { card, pageHeader } from '../../ui/card.js';
import { createListView } from '../../ui/list-view.js';
import { runAction } from '../../ui/actions.js';

export function renderProjectProviders(container, projectId) {
  container.replaceChildren();
  const scope = createScope();

  const header = pageHeader({
    title: t('nav.services'),
  });
  const body = h('div', { className: 'context-panel__body' });
  container.appendChild(header);
  container.appendChild(body);

  const list = createListView({
    scope,
    container: body,
    fetch: () => get(`/admin/api/projects/${projectId}/service-providers`),
    renderItem: (prov) => renderProvider(prov, projectId, () => list.reload()),
    empty: {
      iconName: 'shield',
      title: t('psvc.empty.title'),
      description: t('psvc.empty.description'),
    },
    errorMsg: t('psvc.loadFailed'),
  });
  list.reload();
  return () => scope.dispose();
}

function renderProvider(prov, projectId, reload) {
  const enabledBadge = prov.is_enabled
    ? h('span', { className: 'badge badge--success' }, t('common.enabled'))
    : h('span', { className: 'badge badge--danger' }, t('common.disabled'));

  const secretInfo = prov.secret_name
    ? h('span', { className: 'text-xs text-secondary' }, t('psvc.key', { name: prov.secret_name }))
    : h('span', { className: 'text-xs text-secondary', style: { opacity: 0.5 } }, t('psvc.noAuth'));

  const children = [
    h('div', { style: { padding: '0 16px 12px' } }, [
      prov.description ? h('p', { className: 'text-sm text-secondary mt-1' }, prov.description) : null,
      h('div', { className: 'text-xs text-secondary mt-2' }, [
        h('span', {}, prov.base_url),
        h('span', { style: { margin: '0 8px', opacity: 0.3 } }, '|'),
        h('span', {}, prov.auth_type),
        h('span', { style: { margin: '0 8px', opacity: 0.3 } }, '|'),
        secretInfo,
        prov.allow_private_host
          ? h('span', {
              className: 'badge badge--warning',
              style: { marginInlineStart: '8px' },
              title: t('psvc.lanTip'),
            }, t('psvc.lan'))
          : null,
      ].filter(Boolean)),
    ].filter(Boolean)),
  ];

  if (prov.api_docs) {
    children.push(h('div', { style: { padding: '0 16px 12px' } }, [
      h('details', {}, [
        h('summary', { className: 'text-xs text-secondary', style: { cursor: 'pointer', fontWeight: 600 } }, t('psvc.apiNotes')),
        h('pre', {
          className: 'text-xs',
          style: {
            background: 'var(--bg-secondary)',
            padding: '8px 12px',
            borderRadius: '6px',
            marginTop: '4px',
            whiteSpace: 'pre-wrap',
            wordBreak: 'break-word',
            maxHeight: '200px',
            overflow: 'auto',
          },
        }, prov.api_docs),
      ]),
    ]));
  }

  return card({
    title: prov.name,
    badge: enabledBadge,
    actions: [
      {
        label: prov.is_enabled ? t('tasks.disable') : t('tasks.enable'),
        adminOnly: true,
        className: `btn btn--sm ${prov.is_enabled ? 'btn--secondary' : 'btn--primary'}`,
        onClick: () => runAction(
          () => post(`/admin/api/projects/${projectId}/service-providers/${prov.id}/toggle`),
          { success: prov.is_enabled ? t('common.disabled') : t('common.enabled'), onDone: reload },
        ),
      },
      {
        label: t('common.delete'), kind: 'danger', adminOnly: true,
        onClick: () => modal.confirmDanger(t('psvc.deleteTitle'), t('psvc.deleteBody', { name: prov.name }), () => runAction(
          () => del(`/admin/api/projects/${projectId}/service-providers/${prov.id}`),
          { success: t('psvc.deleted'), onDone: reload },
        )),
      },
    ],
    children,
  });
}
