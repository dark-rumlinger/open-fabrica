/**
 * Project-scoped layouts panel. Hosts two sections, Layouts (full HTML wrapper
 * documents) and Components (reusable fragments resolved by {{include:name}}).
 * Why both here: shared table schema and render-pipeline neighborhood.
 */

import { h } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { get, put, del } from '../../core/http.js';
import * as modal from '../../ui/modal.js';
import { createScope } from '../../core/lifecycle.js';
import { watchToolReload, showLoadError, buildCodeEditorField } from '../../ui/helpers.js';
import { renderResourceGrid } from '../../ui/resource-cards.js';
import { createTabbedPanel } from '../../ui/tabbed-panel.js';
import { runAction } from '../../ui/actions.js';
import { formatDateTime } from '../../core/datetime.js';

export async function renderProjectLayouts(container, projectId, section) {
  const scope = createScope();

  const layoutsBody = h('div');
  const componentsBody = h('div');

  createTabbedPanel({
    container,
    title: t('lay.title'),
    activeSection: section,
    tabs: [
      {
        id: 'layouts',
        label: t('tabs.layouts'),
        content: layoutsBody,
      },
      {
        id: 'components',
        label: t('tabs.components'),
        content: componentsBody,
      },
    ],
  });

  watchToolReload(scope, projectId, { tools: ['manage_files'], actions: ['save', 'delete', 'rename'] }, (evt) => {
    if (evt.args?.kind === 'layout') loadLayouts(layoutsBody, projectId);
    else if (evt.args?.kind === 'component') loadComponents(componentsBody, projectId);
  });

  await Promise.all([
    loadLayouts(layoutsBody, projectId),
    loadComponents(componentsBody, projectId),
  ]);
  return () => scope.dispose();
}

async function loadLayouts(container, projectId) {
  let layouts;
  try { layouts = await get(`/admin/api/projects/${projectId}/layouts`); }
  catch (err) { showLoadError(container, t('lay.loadFailed'), err, () => loadLayouts(container, projectId)); return; }
  renderResourceGrid(container, layouts, {
    projectId,
    getName: (l) => l.name,
    getMeta: (l) => t('mem.updatedAt', { when: formatDateTime(l.updated_at) }),
    getIcon: () => 'layers',
    getBadge: (l) => l.name === 'default' ? { text: t('prov.default'), className: 'badge--info' } : null,
    onEdit: (l) => showHtmlEditor({
      title: t('lay.editLayout', { name: l.name }),
      label: t('lay.templateLabel'),
      filename: 'layout.html',
      minHeight: 400,
      initial: l.template,
      saveUrl: `/admin/api/projects/${projectId}/layouts/${l.id}`,
      saveBody: (content) => ({ template: content }),
      successMsg: t('lay.layoutUpdated'),
      onReload: () => loadLayouts(container, projectId),
    }),
    versionsUrl: (l) => `/admin/api/projects/${projectId}/layouts/${l.id}/versions`,
    canDelete: (l) => l.name !== 'default',
    deleteItem: (l) => del(`/admin/api/projects/${projectId}/layouts/${l.id}`),
    onReload: () => loadLayouts(container, projectId),
    emptyIcon: 'layers',
    emptyTitle: t('lay.empty.title'),
    emptyMessage: t('lay.empty.message'),
    showCreate: false,
    showUpload: false,
  });
}

async function loadComponents(container, projectId) {
  let components;
  try { components = await get(`/admin/api/projects/${projectId}/components`); }
  catch (err) { showLoadError(container, t('lay.componentsFailed'), err, () => loadComponents(container, projectId)); return; }
  renderResourceGrid(container, components, {
    projectId,
    getName: (c) => c.name,
    getMeta: (c) => c.description || t('mem.updatedAt', { when: formatDateTime(c.updated_at) }),
    getIcon: () => 'copy',
    getBadge: () => null,
    onEdit: (c) => showHtmlEditor({
      title: t('lay.editComponent', { name: c.name }),
      label: t('lay.fragmentLabel', { name: c.name }),
      filename: 'component.html',
      minHeight: 300,
      initial: c.body,
      saveUrl: `/admin/api/projects/${projectId}/components/${c.id}`,
      saveBody: (content) => ({ body: content }),
      successMsg: t('lay.componentUpdated'),
      onReload: () => loadComponents(container, projectId),
    }),
    versionsUrl: (c) => `/admin/api/projects/${projectId}/components/${c.id}/versions`,
    canDelete: () => true,
    deleteItem: (c) => del(`/admin/api/projects/${projectId}/components/${c.id}`),
    onReload: () => loadComponents(container, projectId),
    emptyIcon: 'grid',
    emptyTitle: t('lay.componentsEmpty.title'),
    emptyMessage: t('lay.componentsEmpty.message'),
    showCreate: false,
    showUpload: false,
  });
}

async function showHtmlEditor(opts) {
  const field = buildCodeEditorField({
    label: opts.label,
    initial: opts.initial || '',
    filename: opts.filename,
    minHeight: opts.minHeight,
  });

  modal.show(opts.title, field.form, [
    { label: t('common.cancel'), onClick: () => { field.destroy(); } },
    {
      label: t('common.save'),
      className: 'btn btn--primary',
      onClick: () => {
        const content = field.getValue();
        return runAction(() => put(opts.saveUrl, opts.saveBody(content)), {
          success: opts.successMsg,
          onDone: () => { field.destroy(); opts.onReload(); },
        });
      },
    },
  // onClose covers the X and Escape too, which would otherwise leak the editor.
  ], { wide: true, onClose: () => field.destroy() });
}
