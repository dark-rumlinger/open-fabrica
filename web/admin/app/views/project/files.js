import { h, clear } from '../../core/dom.js';
import { t, tn } from '../../core/i18n.js';
import { get, post, put, del } from '../../core/http.js';
import { createScope } from '../../core/lifecycle.js';
import { icon } from '../../ui/icon.js';
import * as modal from '../../ui/modal.js';
import * as toast from '../../ui/toast.js';
import {
  formatBytes,
  watchToolReload,
  showLoadError,
  emptyState,
  clickable,
  formGroup,
} from '../../ui/helpers.js';
import {
  isTextEditable,
  renderResourceCard,
  showUploadModal,
  showCreateModal,
} from '../../ui/resource-cards.js';
import { pageHeader } from '../../ui/card.js';
import { joinFolderPath, listFolderEntries, parentFolder } from './files-tree.js';

export async function renderProjectFiles(container, projectId) {
  clear(container);
  const scope = createScope();
  const browsing = { folder: '', files: [], virtualFolders: new Set() };

  const header = pageHeader({
    title: t('tabs.files'),
  });
  const listContainer = h('div', { className: 'context-panel__body' });
  container.appendChild(header);
  container.appendChild(listContainer);

  watchToolReload(scope, projectId, { tools: ['save_file', 'delete_file'] },
    () => loadFiles(listContainer, projectId, browsing));
  // The builder saving of_test.js refreshes the test script's entry.
  watchToolReload(scope, projectId, { tools: ['manage_testing'], actions: ['write_flows'] },
    () => loadFiles(listContainer, projectId, browsing));

  await loadFiles(listContainer, projectId, browsing);
  return () => scope.dispose();
}

async function loadFiles(container, projectId, browsing) {
  let files;
  try {
    files = await get(`/admin/api/projects/${projectId}/files`);
  } catch (err) {
    showLoadError(container, t('files.loadFailed'), err, () => loadFiles(container, projectId, browsing));
    return;
  }
  browsing.files = files || [];
  renderFolderView(container, projectId, browsing);
}

function renderFolderView(container, projectId, browsing) {
  clear(container);

  const openCreate = () => showCreateModal({
    createTitle: t('files.createTitle'),
    filenamePlaceholder: t('files.filenamePlaceholder'),
    createItem: (filename, content) => post(`/admin/api/projects/${projectId}/files`, {
      filename: joinFolderPath(browsing.folder, filename),
      content,
    }),
    onReload: () => loadFiles(container, projectId, browsing),
  });

  const openUpload = () => showUploadModal({
    uploadTitle: t('files.uploadTitle'),
    uploadUrl: `/admin/api/projects/${projectId}/files`,
    uploadSuccessMsg: t('files.uploaded'),
    showDescription: true,
    extraFormData: () => ({ folder: browsing.folder }),
    onReload: () => loadFiles(container, projectId, browsing),
  });

  if (browsing.files.length === 0 && browsing.virtualFolders.size === 0 && !browsing.folder) {
    container.appendChild(emptyState({
      iconName: 'upload',
      title: t('files.empty.title'),
      description: t('files.empty.message'),
      action: { label: t('rc.create'), onClick: openCreate },
      secondaryAction: { label: t('rc.upload'), onClick: openUpload },
    }));
    return;
  }

  const navigateTo = (folder) => {
    browsing.folder = folder;
    renderFolderView(container, projectId, browsing);
  };

  const openNewFolder = () => showNewFolderModal(browsing.folder, (targetFolder) => {
    browsing.virtualFolders.add(targetFolder);
    navigateTo(targetFolder);
  });

  container.appendChild(renderToolbar(browsing.folder, navigateTo, openNewFolder, openUpload, openCreate));

  const { folders, files } = listFolderEntries(browsing.files, browsing.folder, browsing.virtualFolders);
  if (folders.length === 0 && files.length === 0) {
    container.appendChild(emptyState({
      iconName: 'folder',
      title: t('files.emptyFolder.title'),
      description: t('files.emptyFolder.message'),
    }));
    return;
  }

  // of_test.js is the project's user-flow script: stored apart from the site's
  // files (never published, never loaded by a page) but listed here so the owner
  // can read and edit it. The server checks an edit the way the builder's saves are
  // checked, and refuses to delete it.
  const isTestScript = (f) => f.kind === 'test';
  const cardConfig = {
    projectId,
    getName: (f) => f.displayName || f.filename,
    getTooltip: (f) => f.filename,
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
    onReload: () => loadFiles(container, projectId, browsing),
  };

  const grid = h('div', { className: 'assets-grid' });
  for (const folder of folders) {
    grid.appendChild(renderFolderCard(folder, navigateTo));
  }
  for (const file of files) {
    grid.appendChild(renderResourceCard(file, cardConfig));
  }
  container.appendChild(grid);
}

function renderToolbar(currentFolder, onNavigate, onNewFolder, onUpload, onCreate) {
  const crumbs = [];

  if (currentFolder) {
    crumbs.push(h('button', {
      type: 'button',
      className: 'btn btn--ghost btn--sm',
      title: t('common.back'),
      'aria-label': t('common.back'),
      onClick: () => onNavigate(parentFolder(currentFolder)),
    }, [h('span', { innerHTML: icon('arrow-left') })]));

    crumbs.push(h('button', {
      type: 'button',
      className: 'btn btn--ghost btn--sm',
      onClick: () => onNavigate(''),
    }, t('files.root')));

    const segments = currentFolder.split('/');
    let acc = '';
    for (let i = 0; i < segments.length; i++) {
      const seg = segments[i];
      acc = acc ? `${acc}/${seg}` : seg;
      const target = acc;
      crumbs.push(h('span', {
        className: 'folder-breadcrumbs__sep',
        'aria-hidden': 'true',
        innerHTML: icon('chevron-right'),
      }));
      if (i === segments.length - 1) {
        crumbs.push(h('span', {
          className: 'folder-breadcrumbs__current',
          dir: 'ltr',
        }, seg));
      } else {
        crumbs.push(h('button', {
          type: 'button',
          className: 'btn btn--ghost btn--sm',
          dir: 'ltr',
          onClick: () => onNavigate(target),
        }, seg));
      }
    }
  } else {
    crumbs.push(h('span', { className: 'folder-breadcrumbs__current' }, t('files.root')));
  }

  return h('div', { className: 'files-toolbar' }, [
    h('nav', {
      className: 'folder-breadcrumbs',
      'aria-label': t('files.breadcrumbsLabel'),
    }, crumbs),
    h('div', { className: 'flex items-center gap-2' }, [
      h('button', {
        type: 'button',
        className: 'btn btn--sm btn--secondary',
        onClick: onNewFolder,
      }, [h('span', { innerHTML: icon('folder') }), ' ' + t('files.newFolder')]),
      h('button', {
        type: 'button',
        className: 'btn btn--sm btn--secondary',
        onClick: onUpload,
      }, [h('span', { innerHTML: icon('upload') }), ' ' + t('rc.upload')]),
      h('button', {
        type: 'button',
        className: 'btn btn--primary btn--sm',
        onClick: onCreate,
      }, [h('span', { innerHTML: icon('plus') }), ' ' + t('rc.create')]),
    ]),
  ]);
}

function renderFolderCard(folder, onOpen) {
  const card = h('div', { className: 'asset-card asset-card--folder' }, [
    h('div', { className: 'asset-card__preview' }, [
      h('div', { className: 'asset-card__img-placeholder' }, [
        h('span', { innerHTML: icon('folder') }),
      ]),
    ]),
    h('div', { className: 'asset-card__info' }, [
      h('span', { className: 'asset-card__name', dir: 'ltr', title: folder.path }, folder.name),
      h('span', { className: 'text-xs text-secondary' }, tn('files.folderCount', folder.count)),
    ]),
  ]);
  clickable(card, () => onOpen(folder.path));
  return card;
}

function showNewFolderModal(currentFolder, onCreated) {
  const input = h('input', {
    className: 'input',
    type: 'text',
    dir: 'ltr',
    placeholder: t('files.folderPlaceholder'),
  });
  const form = h('div', {}, [formGroup(t('files.folderName'), input)]);

  modal.show(t('files.newFolder'), form, [
    modal.cancelBtn(),
    {
      label: t('rc.create'),
      className: 'btn btn--primary',
      onClick: () => {
        const joined = joinFolderPath(currentFolder, input.value);
        const invalid = !joined || joined.split('/').some((seg) => seg === '.' || seg === '..');
        if (invalid) {
          toast.error(t('files.folderInvalid'));
          return false;
        }
        onCreated(joined);
      },
    },
  ]);
}
