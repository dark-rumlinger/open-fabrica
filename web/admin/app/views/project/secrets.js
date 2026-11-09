import { h } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { get, post, put, del } from '../../core/http.js';
import { card } from '../../ui/card.js';
import { createCrudPanel, confirmDelete } from '../../ui/crud-panel.js';
import { formGroup, readonlyField } from '../../ui/helpers.js';
import { createFormModal } from '../../ui/form-modal.js';
import { formatDateTime } from '../../core/datetime.js';

export function renderProjectSecrets(container, projectId) {
  const panel = createCrudPanel({
    container,
    projectId,
    title: t('tabs.apiKeys'),
    createLabel: t('sec.add'),
    onCreate: (reload) => showCreateModal(projectId, reload),
    note: t('sec.note'),
    fetch: () => get(`/admin/api/projects/${projectId}/secrets`),
    renderItem: (s, { reload }) => renderSecret(s, projectId, reload),
    filter: true,
    empty: {
      iconName: 'lock',
      title: t('sec.empty.title'),
      description: t('sec.empty.description'),
    },
    errorMsg: t('sec.loadFailed'),
    toolReload: { tools: ['manage_secrets'], actions: ['store', 'delete'] },
  });
  return () => panel.dispose();
}

function renderSecret(s, projectId, reload) {
  return card({
    title: s.name,
    iconName: 'lock',
    // An imported project keeps its key names but not their values; flag those so
    // the owner knows which ones still need filling in. Absent means unknown, not empty.
    badge: s.has_value === false ? h('span', { className: 'badge badge--warning' }, t('sec.needsValue')) : undefined,
    actions: [
      { label: t('common.edit'), iconName: 'edit', adminOnly: true, onClick: () => showEditModal(projectId, s, reload) },
      { label: t('common.delete'), iconName: 'trash', kind: 'danger', adminOnly: true, onClick: () => confirmDeleteSecret(projectId, s, reload) },
    ],
    children: [
      h('div', { className: 'text-xs text-secondary', style: { padding: '0 12px 12px' } },
        t('mem.updatedAt', { when: formatDateTime(s.updated_at) })),
    ],
  });
}

function valueInputWithToggle(placeholder = t('sec.valuePlaceholder')) {
  const input = h('input', { className: 'input', type: 'password', placeholder });
  const toggle = h('button', {
    className: 'btn btn--ghost btn--sm',
    type: 'button',
    onClick: () => {
      input.type = input.type === 'password' ? 'text' : 'password';
      toggle.textContent = input.type === 'password' ? t('tg.show') : t('tg.hide');
    },
  }, t('tg.show'));
  return { input, group: h('div', { className: 'flex gap-2' }, [input, toggle]) };
}

function showCreateModal(projectId, reload) {
  const nameInput = h('input', { className: 'input', placeholder: 'stripe_key, openai_api_key, db_password' });
  const v = valueInputWithToggle();

  createFormModal({
    title: t('sec.add'),
    fields: [
      formGroup(t('projects.col.name'), nameInput, {
        hint: t('sec.nameHint'),
      }),
      formGroup(t('mem.value'), v.group, {
        hint: t('sec.valueHint'),
      }),
    ],
    submitLabel: t('sec.add'),
    validate: () => (!nameInput.value.trim() || !v.input.value) ? t('sec.required') : null,
    submit: () => post(`/admin/api/projects/${projectId}/secrets`, { name: nameInput.value.trim(), value: v.input.value }),
    success: t('sec.added'),
    onDone: reload,
  });
}

function showEditModal(projectId, secret, reload) {
  const v = valueInputWithToggle(t('sec.newValue'));
  createFormModal({
    title: t('sec.editTitle', { name: secret.name }),
    fields: [
      readonlyField(t('projects.col.name'), secret.name),
      formGroup(t('sec.newValue'), v.group),
    ],
    validate: () => (!v.input.value ? t('sec.valueRequired') : null),
    submit: () => put(`/admin/api/projects/${projectId}/secrets/${secret.id}`, { value: v.input.value }),
    success: t('sec.updated'),
    onDone: reload,
  });
}

function confirmDeleteSecret(projectId, s, reload) {
  confirmDelete({
    title: t('sec.deleteTitle'),
    message: t('sec.deleteBody', { name: s.name }),
    run: () => del(`/admin/api/projects/${projectId}/secrets/${s.id}`),
    success: t('sec.deleted'),
    onDone: reload,
  });
}
