import { h, clear } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { get, post, put, del } from '../../core/http.js';
import { createScope } from '../../core/lifecycle.js';
import { watchToolReload, showLoadError } from '../../ui/helpers.js';
import { renderResourceGrid, showCreateModal } from '../../ui/resource-cards.js';
import { pageHeader } from '../../ui/card.js';
import { formatDate } from '../../core/datetime.js';

export async function renderProjectPages(container, projectId) {
  clear(container);
  const scope = createScope();

  const header = pageHeader({
    title: t('tabs.pages'),
  });
  const listContainer = h('div', { className: 'context-panel__body' });
  container.appendChild(header);
  container.appendChild(listContainer);

  watchToolReload(scope, projectId, { tools: ['save_page', 'delete_page', 'restore_page'] },
    () => loadPages(listContainer, projectId));

  await loadPages(listContainer, projectId);
  return () => scope.dispose();
}

async function loadPages(container, projectId) {
  let pages;
  try {
    pages = await get(`/admin/api/projects/${projectId}/pages`);
  } catch (err) {
    showLoadError(container, t('pages.loadFailed'), err, () => loadPages(container, projectId));
    return;
  }
  renderResourceGrid(container, pages, {
    projectId,
    getName: (p) => p.path,
    getMeta: (p) => `${p.title || '--'} - ${formatDate(p.updated_at)}`,
    getIcon: () => 'file-text',
    isEditable: () => true,
    editorFilename: () => 'page.html',
    contentUrl: (p) => `/admin/api/projects/${projectId}/pages/${p.id}`,
    contentExtractor: (json) => json.content || '',
    saveContent: async (p, content) => { await put(`/admin/api/projects/${projectId}/pages/${p.id}`, { content }); },
    versionsUrl: (p) => `/admin/api/projects/${projectId}/pages/${p.id}/versions`,
    deleteItem: (p) => del(`/admin/api/projects/${projectId}/pages/${p.id}`),
    onReload: () => loadPages(container, projectId),
    emptyIcon: 'file-text',
    emptyTitle: t('pages.empty.title'),
    emptyMessage: t('pages.empty.message'),
    showCreate: true,
    showUpload: false,
    onCreate: () => showCreateModal({
      createTitle: t('pages.createTitle'),
      filenameLabel: t('pages.path'),
      filenamePlaceholder: t('pages.pathPlaceholder'),
      createItem: (path, content) => post(`/admin/api/projects/${projectId}/pages`, {
        path: path.startsWith('/') ? path : '/' + path,
        content,
      }),
      onReload: () => loadPages(container, projectId),
    }),
  });
}
