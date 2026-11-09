/**
 * Shared resource card grid component for Pages and Files.
 * Provides unified card layout, edit modal, version history, upload, and create.
 */

import { h, clear } from '../core/dom.js';
import { t } from '../core/i18n.js';
import { get, post, rawFetch } from '../core/http.js';
import * as state from '../core/state.js';
import { icon } from './icon.js';
import * as toast from './toast.js';
import * as modal from './modal.js';
import { emptyState, formGroup, buildCodeEditorField } from './helpers.js';
import { formatDateTime } from '../core/datetime.js';
import { humanizeError } from './errors.js';

const TEXT_EXTENSIONS = new Set([
  'txt', 'csv', 'css', 'js', 'jsx', 'ts', 'tsx', 'json', 'xml', 'svg',
  'html', 'htm', 'md', 'yaml', 'yml', 'toml', 'env', 'cfg', 'ini',
  'sh', 'bat', 'sql', 'graphql', 'py', 'rb', 'php', 'go', 'rs', 'lua',
]);

const TEXT_CONTENT_TYPES = new Set([
  'application/javascript', 'application/json', 'application/xml',
  'image/svg+xml', 'application/x-yaml', 'application/toml',
]);

/**
 * Check if a file is editable as text.
 */
export function isTextEditable(contentType, filename) {
  if (filename) {
    const ext = filename.split('.').pop().toLowerCase();
    if (TEXT_EXTENSIONS.has(ext)) return true;
  }
  if (!contentType) return false;
  if (contentType.startsWith('text/')) return true;
  return TEXT_CONTENT_TYPES.has(contentType);
}

/**
 * Render a resource grid with action bar and cards.
 *
 * @param {HTMLElement} container
 * @param {Array} items
 * @param {Object} config
 * @param {string} config.projectId
 * @param {Function} config.getName - (item) => display name
 * @param {Function} config.getMeta - (item) => secondary text
 * @param {Function} config.getIcon - (item) => icon name
 * @param {Function} [config.getBadge] - (item) => { text, className } or null
 * @param {Function} [config.isEditable] - (item) => boolean
 * @param {Function} [config.contentUrl] - (item) => URL to fetch content
 * @param {Function} [config.saveContent] - (item, content) => Promise
 * @param {Function} [config.versionsUrl] - (item) => URL or null
 * @param {Function} config.deleteItem - (item) => Promise
 * @param {Function} config.onReload - () => reload list
 * @param {string} config.emptyMessage
 * @param {string} [config.emptyTitle]
 * @param {string} [config.emptyIcon]
 * @param {boolean} [config.showCreate]
 * @param {boolean} [config.showUpload]
 * @param {Function} [config.onCreate] - () => show create modal
 * @param {Function} [config.onUpload] - () => show upload modal
 */
export function renderResourceGrid(container, items, config) {
  clear(container);

  // Nothing to show: the empty state carries the action on its own, rather
  // than a lone button floating at the top of a blank page while the message
  // underneath asks you to add something and offers no way to.
  // Writes are admin-only server-side, so non-admins get no add controls.
  const canWrite = state.isAdmin();
  const canCreate = canWrite && config.showCreate && config.onCreate;
  const canUpload = canWrite && config.showUpload && config.onUpload;

  if (!items || items.length === 0) {
    const primary = canCreate
      ? { label: t('rc.create'), onClick: config.onCreate }
      : canUpload
        ? { label: t('rc.upload'), onClick: config.onUpload }
        : null;
    // Both are on offer (Files): an image-first user needs Upload as much as a
    // text-first user needs Create.
    const secondary = (canCreate && canUpload) ? { label: t('rc.upload'), onClick: config.onUpload } : null;
    container.appendChild(emptyState({
      iconName: config.emptyIcon || 'list',
      title: config.emptyTitle || t('list.emptyTitle'),
      description: config.emptyMessage || '',
      action: primary,
      secondaryAction: secondary,
    }));
    return;
  }

  // Action bar, pushed right and toned down: with a grid of cards below it,
  // adding another is a tool rather than the point of the page.
  const buttons = [];
  if (canUpload) {
    buttons.push(h('button', {
      className: 'btn btn--sm btn--secondary',
      onClick: config.onUpload,
    }, [h('span', { innerHTML: icon('upload') }), ' ' + t('rc.upload')]));
  }
  if (canCreate) {
    buttons.push(h('button', {
      className: 'btn btn--primary btn--sm',
      onClick: config.onCreate,
    }, [h('span', { innerHTML: icon('plus') }), ' ' + t('rc.create')]));
  }
  if (buttons.length > 0) {
    container.appendChild(h('div', { className: 'flex items-center gap-2 mb-3 justify-end' }, buttons));
  }

  const grid = h('div', { className: 'assets-grid' });

  for (const item of items) {
    grid.appendChild(renderResourceCard(item, config));
  }

  container.appendChild(grid);
}

/**
 * Render a single resource card.
 */
function renderResourceCard(item, config) {
  const name = config.getName(item);
  const meta = config.getMeta(item);
  const iconName = config.getIcon(item);
  const editable = config.isEditable ? config.isEditable(item) : false;
  const badge = config.getBadge ? config.getBadge(item) : null;
  const canViewVersions = config.canViewVersions ? config.canViewVersions(item) : true;
  const hasVersions = item.version_count > 0 && config.versionsUrl && canViewVersions;

  const actionBtns = [];
  const canWrite = state.isAdmin();

  if (!canWrite) {
    // read-only: no edit action
  } else if (config.onEdit) {
    actionBtns.push(h('button', {
      className: 'btn btn--ghost btn--sm',
      title: t('common.edit'),
      'aria-label': t('common.edit'),
      onClick: (e) => {
        e.stopPropagation();
        config.onEdit(item);
      },
    }, [h('span', { innerHTML: icon('edit') })]));
  } else if (editable && config.contentUrl) {
    actionBtns.push(h('button', {
      className: 'btn btn--ghost btn--sm',
      title: t('common.edit'),
      'aria-label': t('common.edit'),
      onClick: (e) => {
        e.stopPropagation();
        showEditModal(item, config);
      },
    }, [h('span', { innerHTML: icon('edit') })]));
  }

  if (hasVersions) {
    actionBtns.push(h('button', {
      className: 'btn btn--ghost btn--sm',
      title: t('rc.versionHistory'),
      'aria-label': t('rc.versionHistory'),
      onClick: (e) => {
        e.stopPropagation();
        showVersionHistory(item, config);
      },
    }, [h('span', { innerHTML: icon('clock') })]));
  }

  const canDelete = canWrite && (config.canDelete ? config.canDelete(item) : true);
  if (canDelete) {
    actionBtns.push(h('button', {
      className: 'btn btn--ghost btn--sm',
      title: t('common.delete'),
      'aria-label': t('common.delete'),
      onClick: (e) => {
        e.stopPropagation();
        confirmDelete(item, config);
      },
    }, [h('span', { innerHTML: icon('trash') })]));
  }

  const previewChildren = [
    h('div', { className: 'asset-card__img-placeholder' }, [
      h('span', { innerHTML: icon(iconName) }),
    ]),
  ];
  if (badge) {
    previewChildren.push(
      h('span', { className: `badge badge--sm ${badge.className || ''}` }, badge.text)
    );
  }

  return h('div', { className: 'asset-card' }, [
    h('div', { className: 'asset-card__preview' }, previewChildren),
    h('div', { className: 'asset-card__info' }, [
      h('span', { className: 'asset-card__name' }, name),
      h('span', { className: 'text-xs text-secondary' }, meta),
    ]),
    h('div', { className: 'asset-card__actions' }, actionBtns),
  ]);
}

/**
 * Show a wide modal with the code editor for editing a resource.
 */
async function showEditModal(item, config) {
  const name = config.getName(item);
  let content = '';
  try {
    const res = await rawFetch(config.contentUrl(item));
    if (config.contentExtractor) {
      const json = await res.json();
      content = config.contentExtractor(json);
    } else {
      content = await res.text();
    }
  } catch (err) {
    // Opening an empty editor here would let a Save overwrite the real file
    // with nothing, so stop instead.
    toast.error(t('rc.openFailed', { name, error: humanizeError(err.message) }));
    return;
  }

  const editorFilename = config.editorFilename ? config.editorFilename(item) : name;
  const field = buildCodeEditorField({ label: name, initial: content, filename: editorFilename, minHeight: 350 });

  modal.show(t('rc.editTitle', { name }), field.form, [
    { label: t('common.cancel'), onClick: () => { field.destroy(); } },
    {
      label: t('common.save'),
      className: 'btn btn--primary',
      onClick: async () => {
        const newContent = field.getValue();
        try {
          await config.saveContent(item, newContent);
          toast.success(t('rc.updated', { name }));
          field.destroy();
          config.onReload();
        } catch (err) {
          toast.error(humanizeError(err.message));
          return false;
        }
      },
    },
  // onClose covers the X and Escape as well as Cancel and Save.
  ], { wide: true, onClose: () => { field.destroy(); } });
}

/**
 * Show version history modal: who changed the resource and when, with a Restore
 * button per older version for admins. Restore posts to the sibling `/restore`
 * route of the versions URL, which re-saves that snapshot as a new version, so a
 * restore is itself undoable. Old bodies are pruned, and the server answers with
 * the reason when a version can no longer be restored.
 */
async function showVersionHistory(item, config) {
  const name = config.getName(item);
  let versions;
  try {
    versions = await get(config.versionsUrl(item));
  } catch (err) {
    toast.error(t('rc.historyFailed', { error: humanizeError(err.message) }));
    return;
  }

  if (!versions || versions.length === 0) {
    toast.info(t('rc.noHistory', { name }));
    return;
  }

  const restoreUrl = restoreUrlFor(config.versionsUrl(item));
  const canRestore = state.isAdmin();

  const list = h('div', { className: 'version-list' });
  versions.forEach((v, i) => {
    const date = formatDateTime(v.created_at);
    const cells = [
      h('span', { className: 'badge badge--sm' }, `v${v.version_number}`),
      h('span', { className: 'text-sm text-secondary', style: { flex: '1' } },
        `${v.changed_by} \u00b7 ${date}`),
    ];
    // The newest row is the live version; restoring it would change nothing.
    if (canRestore && i > 0) {
      const btn = h('button', { className: 'btn btn--ghost btn--sm' }, t('rc.restore'));
      btn.addEventListener('click', () => confirmRestore(name, v, restoreUrl, config));
      cells.push(btn);
    }
    list.appendChild(h('div', {
      className: 'version-row flex items-center gap-2',
      style: { padding: '8px 0', borderBottom: '1px solid var(--border)' },
    }, cells));
  });

  modal.show(t('rc.historyTitle', { name }), list, [
    modal.cancelBtn(t('common.close')),
  ]);
}

/**
 * The restore route is the sibling of the versions route for every artifact kind
 * (`.../files/7/versions` -> `.../files/7/restore`).
 */
export function restoreUrlFor(versionsUrl) {
  return versionsUrl.replace(/\/versions$/, '/restore');
}

/**
 * Confirm and run a restore of one version. Closes the dialog on success and
 * reloads the list; a failure keeps it open with the server's reason.
 */
function confirmRestore(name, v, restoreUrl, config) {
  modal.show(t('rc.restoreTitle'), h('p', {}, t('rc.restoreBody', { name, version: v.version_number })), [
    modal.cancelBtn(),
    {
      label: t('rc.restore'),
      className: 'btn btn--primary',
      onClick: async () => {
        try {
          await post(restoreUrl, { version: v.version_number });
          toast.success(t('rc.restored', { name, version: v.version_number }));
          config.onReload();
        } catch (err) {
          toast.error(t('rc.restoreFailed', { error: humanizeError(err.message) }));
          return false;
        }
      },
    },
  ]);
}

/**
 * Show a delete confirmation modal.
 */
function confirmDelete(item, config) {
  const name = config.getName(item);
  // The shared danger-confirm (focus trap, cancel focused by default, danger
  // styling) rather than a bespoke modal.show, so a page/file/layout/component
  // delete reads and behaves like every other destructive action.
  modal.confirmDanger(t('common.delete'), t('rc.deleteBody', { name }), async () => {
    try {
      await config.deleteItem(item);
      toast.success(t('rc.deleted', { name }));
      config.onReload();
    } catch (err) {
      toast.error(humanizeError(err.message));
      return false;
    }
  });
}

/**
 * Show upload modal with file input and optional description.
 */
export function showUploadModal(config) {
  const fileInput = h('input', { type: 'file', className: 'input' });
  const descInput = config.showDescription ? h('input', {
    type: 'text',
    className: 'input',
    placeholder: t('rc.descPlaceholder'),
  }) : null;

  const fields = [formGroup(t('rc.file'), fileInput)];
  if (descInput) fields.push(formGroup(t('rc.description'), descInput));

  const form = h('div', {}, fields);

  modal.show(config.uploadTitle || t('rc.upload'), form, [
    modal.cancelBtn(),
    {
      label: t('rc.upload'),
      className: 'btn btn--primary',
      onClick: async () => {
        const file = fileInput.files && fileInput.files[0];
        if (!file) {
          toast.error(t('rc.chooseFile'));
          return false;
        }

        const formData = new FormData();
        formData.append('file', file);
        if (descInput && descInput.value.trim()) {
          formData.append('description', descInput.value.trim());
        }

        try {
          // FormData: rawFetch sets only Authorization; the browser sets the
          // multipart Content-Type with its boundary.
          await rawFetch(config.uploadUrl, { method: 'POST', body: formData });
          toast.success(config.uploadSuccessMsg || t('rc.uploaded'));
          config.onReload();
        } catch (err) {
          toast.error(humanizeError(err.message));
          return false;
        }
      },
    },
  ]);
}

/**
 * Show create modal with filename input and the code editor.
 */
export function showCreateModal(config) {
  const filenameInput = h('input', {
    className: 'input',
    type: 'text',
    placeholder: config.filenamePlaceholder || t('rc.filenamePlaceholder'),
  });
  const editorContainer = h('div', { style: { marginTop: '8px' } });
  let editor = null;

  // Sequence guard: two quick filename changes must not leave two editors
  // attached, and whatever was typed so far carries over to the new editor.
  let swapSeq = 0;
  filenameInput.addEventListener('change', async () => {
    const fn = filenameInput.value.trim();
    const seq = ++swapSeq;
    const carried = editor ? editor.getValue() : fallbackArea.value;
    const { createEditor } = await import('./code-editor.js');
    if (seq !== swapSeq) return;
    if (editor) editor.destroy();
    editorContainer.innerHTML = '';
    const next = await createEditor(editorContainer, { filename: fn, value: carried, minHeight: 250 });
    if (seq !== swapSeq) { next.destroy(); return; }
    editor = next;
  });

  const fallbackArea = h('textarea', {
    className: 'input',
    rows: 10,
    style: { fontFamily: 'var(--font-mono)', fontSize: 'var(--text-sm)', tabSize: '2' },
    placeholder: t('rc.highlightHint'),
  });
  editorContainer.appendChild(fallbackArea);

  const form = h('div', {}, [
    formGroup(config.filenameLabel || t('rc.filename'), filenameInput),
    formGroup(t('rc.content'), editorContainer),
  ]);

  modal.show(config.createTitle || t('rc.create'), form, [
    { label: t('common.cancel'), onClick: () => { if (editor) editor.destroy(); } },
    {
      label: t('rc.create'),
      className: 'btn btn--primary',
      onClick: async () => {
        const filename = filenameInput.value.trim();
        if (!filename) {
          toast.error(t('rc.filenameRequired'));
          return false;
        }
        const content = editor ? editor.getValue() : fallbackArea.value;
        try {
          await config.createItem(filename, content);
          toast.success(t('rc.created', { name: filename }));
          if (editor) editor.destroy();
          config.onReload();
        } catch (err) {
          toast.error(humanizeError(err.message));
          return false;
        }
      },
    },
  ], { wide: true, onClose: () => { if (editor) editor.destroy(); } });
}
