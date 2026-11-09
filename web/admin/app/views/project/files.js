import { h, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { get, post, put, del } from '../../core/http.js';
import { createScope } from '../../core/lifecycle.js';
import { formatBytes, watchToolReload, showLoadError } from '../../ui/helpers.js';
import {
  isTextEditable,
  renderResourceGrid,
  showUploadModal,
  showCreateModal,
} from '../../ui/resource-cards.js';
import { pageHeader } from '../../ui/card.js';

export async function renderProjectFiles(container, projectId) {
  clear(container);
  const scope = createScope();

  const header = pageHeader({
    title: t('tabs.files'),
  });
  const listContainer = h('div', { className: 'context-panel__body' });
  container.appendChild(header);
  container.appendChild(listContainer);

  watchToolReload(scope, projectId, { tools: ['save_file', 'delete_file'] },
    () => loadFiles(listContainer, projectId));
  // The builder saving of_test.js refreshes the test script's entry.
  watchToolReload(scope, projectId, { tools: ['manage_testing'], actions: ['write_flows'] },
    () => loadFiles(listContainer, projectId));

  await loadFiles(listContainer, projectId);
  return () => scope.dispose();
}

async function loadFiles(container, projectId) {
  let files;
  try {
    files = await get(`/admin/api/projects/${projectId}/files`);
  } catch (err) {
    showLoadError(container, t('files.loadFailed'), err, () => loadFiles(container, projectId));
    return;
  }
  // of_test.js is the project's user-flow script: stored apart from the site's
  // files (never published, never loaded by a page) but listed here so the owner
  // can read and edit it. The server checks an edit the way the builder's saves are
  // checked, and refuses to delete it.
  const isTestScript = (f) => f.kind === 'test';
  renderResourceGrid(container, files, {
    projectId,
    getName: (f) => f.filename,
    getMeta: (f) => {
      const parts = [`${f.content_type || t('files.unknownType')} - ${f.size ? formatBytes(f.size) : '--'}`];
      if (isTestScript(f)) parts.push(t('files.testScript'));
      else if (f.platform_managed) parts.push(t('files.managed'));
      else if (f.description) parts.push(f.description);
      return parts.join(' - ');
    },
    getIcon: (f) => f.content_type?.startsWith('image/') ? 'image' : 'file',
    // Platform-managed files are OpenFabrica-owned: badge them, and hide
    // edit/delete/versions since the server rejects those writes anyway.
    getBadge: (f) => {
      if (isTestScript(f)) return { text: t('files.flowTests'), className: 'badge--info' };
      return f.platform_managed ? { text: t('files.protected'), className: 'badge--info' } : null;
    },
    canDelete: (f) => !f.platform_managed && !isTestScript(f),
    canViewVersions: (f) => !f.platform_managed,
    isEditable: (f) => isTestScript(f) || (!f.platform_managed && isTextEditable(f.content_type, f.filename)),
    contentUrl: (f) => `/admin/api/projects/${projectId}/files/${f.id}/content`,
    saveContent: (f, content) => put(`/admin/api/projects/${projectId}/files/${f.id}/content`, { content }),
    versionsUrl: (f) => `/admin/api/projects/${projectId}/files/${f.id}/versions`,
    deleteItem: (f) => del(`/admin/api/projects/${projectId}/files/${f.id}`),
    onReload: () => loadFiles(container, projectId),
    emptyIcon: 'upload',
    emptyTitle: t('files.empty.title'),
    emptyMessage: t('files.empty.message'),
    showCreate: true,
    showUpload: true,
    onCreate: () => showCreateModal({
      createTitle: t('files.createTitle'),
      filenamePlaceholder: t('files.filenamePlaceholder'),
      createItem: (filename, content) => post(`/admin/api/projects/${projectId}/files`, { filename, content }),
      onReload: () => loadFiles(container, projectId),
    }),
    onUpload: () => showUploadModal({
      uploadTitle: t('files.uploadTitle'),
      uploadUrl: `/admin/api/projects/${projectId}/files`,
      uploadSuccessMsg: t('files.uploaded'),
      showDescription: true,
      onReload: () => loadFiles(container, projectId),
    }),
  });
}
