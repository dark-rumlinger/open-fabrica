import { h } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { get, post, put, del } from '../../core/http.js';
import { card } from '../../ui/card.js';
import { createCrudPanel, confirmDelete } from '../../ui/crud-panel.js';
import { formGroup, readonlyField } from '../../ui/helpers.js';
import { createFormModal } from '../../ui/form-modal.js';
import { formatDateTime } from '../../core/datetime.js';

export function renderProjectMemory(container, projectId) {
  const panel = createCrudPanel({
    container,
    projectId,
    title: t('tabs.memory'),
    createLabel: t('mem.add'),
    onCreate: (reload) => showCreateModal(projectId, reload),
    fetch: () => get(`/admin/api/projects/${projectId}/memory`),
    renderItem: (m, { reload }) => renderMemory(m, projectId, reload),
    empty: {
      iconName: 'bookmark',
      title: t('mem.empty.title'),
      description: t('mem.empty.description'),
    },
    errorMsg: t('mem.loadFailed'),
    filter: true,
    // Live-refresh when the engine writes memory via manage_memory.
    toolReload: { tools: ['manage_memory'], actions: ['store', 'forget'] },
  });
  return () => panel.dispose();
}

function renderMemory(m, projectId, reload) {
  const badge = m.category
    ? h('span', { className: 'badge' }, m.category)
    : null;
  const actions = [
    { label: t('common.edit'), iconName: 'edit', adminOnly: true, onClick: () => showEditModal(projectId, m, reload) },
    { label: t('common.delete'), iconName: 'trash', kind: 'danger', adminOnly: true, onClick: () => confirmDeleteMemory(projectId, m, reload) },
  ];
  return card({
    title: m.key,
    badge,
    iconName: 'bookmark',
    actions,
    children: [
      h('div', { className: 'text-sm', style: { padding: '0 12px 8px', whiteSpace: 'pre-wrap' } }, m.value),
      h('div', { className: 'text-xs text-secondary', style: { padding: '0 12px 12px' } },
        t('mem.updatedAt', { when: formatDateTime(m.updated_at) })),
    ],
  });
}

function showCreateModal(projectId, reload) {
  const keyInput = h('input', { className: 'input', placeholder: 'brand_color, business_hours, default_theme' });
  const valueInput = h('textarea', { className: 'input', rows: 3, placeholder: t('mem.valuePlaceholder') });
  const categoryInput = h('input', { className: 'input', placeholder: 'general', value: 'general' });

  createFormModal({
    title: t('mem.add'),
    fields: [
      formGroup(t('mem.key'), keyInput, { hint: t('mem.keyHint') }),
      formGroup(t('mem.value'), valueInput, { hint: t('mem.valueHint') }),
      formGroup(t('mem.category'), categoryInput, { hint: t('mem.categoryHint') }),
    ],
    submitLabel: t('mem.add'),
    validate: () => (!keyInput.value.trim() || !valueInput.value.trim()) ? t('mem.required') : null,
    submit: () => post(`/admin/api/projects/${projectId}/memory`, {
      key: keyInput.value.trim(),
      value: valueInput.value.trim(),
      category: categoryInput.value.trim() || 'general',
    }),
    success: t('mem.added'),
    onDone: reload,
  });
}

function showEditModal(projectId, m, reload) {
  const valueInput = h('textarea', { className: 'input', rows: 3, value: m.value });
  const categoryInput = h('input', { className: 'input', value: m.category || 'general' });

  createFormModal({
    title: t('mem.editTitle', { key: m.key }),
    fields: [
      readonlyField(t('mem.key'), m.key),
      formGroup(t('mem.value'), valueInput),
      formGroup(t('mem.category'), categoryInput),
    ],
    validate: () => (!valueInput.value.trim() ? t('mem.valueRequired') : null),
    submit: () => put(`/admin/api/projects/${projectId}/memory/${m.id}`, {
      value: valueInput.value.trim(), category: categoryInput.value.trim() || 'general',
    }),
    success: t('mem.updated'),
    onDone: reload,
  });
}

function confirmDeleteMemory(projectId, m, reload) {
  confirmDelete({
    title: t('mem.deleteTitle'),
    message: t('mem.deleteBody', { key: m.key }),
    run: () => del(`/admin/api/projects/${projectId}/memory/${m.id}`),
    success: t('mem.deleted'),
    onDone: reload,
  });
}
