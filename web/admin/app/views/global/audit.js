/**
 * Admin audit log: who changed what in the control plane, newest first. Read-only
 * and admin-only (the endpoint is gated); the control plane can read and decrypt
 * every project, so this is the first place to look after an unexpected change.
 */

import { h, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { get } from '../../core/http.js';
import { defineView } from '../../core/view.js';
import { createListView } from '../../ui/list-view.js';
import { pageHeader, pageBody } from '../../ui/card.js';
import { formatDateTime } from '../../core/datetime.js';

export const renderAudit = defineView((ctx, container) => {
  const { scope } = ctx;
  clear(container);

  container.appendChild(pageHeader({ title: t('nav.audit') }));
  const body = pageBody();
  const listContainer = h('div');
  body.appendChild(listContainer);
  container.appendChild(body);

  const list = createListView({
    scope,
    container: listContainer,
    filter: true,
    fetch: async () => {
      const res = await get('/admin/api/system/audit?limit=200');
      return res.items || [];
    },
    renderItem: (row) => h('tr', {}, [
      h('td', { dataset: { label: t('audit.col.when') } }, formatDateTime(row.created_at)),
      h('td', { dataset: { label: t('audit.col.who') } }, row.username || ('#' + row.user_id)),
      h('td', { dataset: { label: t('audit.col.action') } }, [
        h('span', { className: 'badge badge--neutral' }, row.method),
        ' ',
        h('span', { className: 'text-xs', style: { fontFamily: 'var(--font-mono)' } }, row.path),
      ]),
      h('td', { dataset: { label: t('audit.col.target') } }, row.target_project_id ? ('#' + row.target_project_id) : '—'),
      h('td', {
        dataset: { label: t('audit.col.status') },
        // The request id is the same reference an error state shows, so an audit
        // row can be matched to a failure report; kept as a tooltip to stay terse.
        title: row.request_id || '',
      }, [statusBadge(row.status)]),
    ]),
    wrap: (rows) => h('div', { className: 'table-wrapper table-wrapper--responsive' }, [
      h('table', { className: 'table' }, [
        h('thead', {}, [h('tr', {}, [
          h('th', {}, t('audit.col.when')),
          h('th', {}, t('audit.col.who')),
          h('th', {}, t('audit.col.action')),
          h('th', {}, t('audit.col.target')),
          h('th', {}, t('audit.col.status')),
        ])]),
        h('tbody', {}, rows),
      ]),
    ]),
    empty: { iconName: 'list', title: t('audit.empty') },
    errorMsg: t('list.loadFailed'),
  });
  list.reload();
});

function statusBadge(status) {
  const cls = status >= 400 ? 'badge--danger' : 'badge--success';
  return h('span', { className: 'badge ' + cls }, String(status));
}
