/**
 * CRUD-panel scaffold: the pageHeader -> toolbar(create) -> list-view ->
 * watchToolReload wiring every project CRUD view repeats. View-specific parts
 * (item card, create/edit modals) stay with the view and come in as
 * callbacks. Options: container, projectId, title, createLabel,
 * onCreate, note, fetch, renderItem, empty, errorMsg, toolReload.
 */

import { h } from '../core/dom.js';
import { createScope } from '../core/lifecycle.js';
import { pageHeader } from './card.js';
import { createListView } from './list-view.js';
import { watchToolReload } from './helpers.js';
import * as modal from './modal.js';
import { runAction } from './actions.js';
import * as state from '../core/state.js';

export function createCrudPanel(opts) {
  const {
    container,
    scope: providedScope,
    projectId,
    title,
    createLabel,
    onCreate,
    note,
    toolbarExtra,
    fetch,
    renderItem,
    empty,
    errorMsg,
    toolReload,
    filter,
  } = opts;

  if (!container) throw new Error('createCrudPanel: container required');

  const scope = providedScope || createScope();
  container.replaceChildren();

  container.appendChild(pageHeader({ title }));
  const body = h('div', { className: 'context-panel__body list-block' });
  container.appendChild(body);

  // Toolbar: an optional inline note, any caller-supplied control, then the
  // create button pushed to the right. The create button is the page's one
  // primary action; while the list is empty the empty state carries it instead
  // (see .list-toolbar). Skipped entirely when there's nothing to put in it.
  const toolbarChildren = [];
  if (note) {
    toolbarChildren.push(h('span', { className: 'notice text-sm text-secondary', style: { marginTop: '0' } }, note));
  }
  if (toolbarExtra) toolbarChildren.push(toolbarExtra);
  // Writes are admin-only server-side, so non-admins get no create control
  // that could only answer 403.
  const canCreate = !!(createLabel && onCreate && state.isAdmin());
  if (canCreate) {
    toolbarChildren.push(h('button', {
      className: 'btn btn--primary btn--sm ml-auto',
      onClick: () => onCreate(() => list.reload()),
    }, createLabel));
  }
  if (toolbarChildren.length) {
    body.appendChild(h('div', { className: 'list-toolbar flex items-center gap-2 mb-3' }, toolbarChildren));
  }

  const listEl = h('div');
  body.appendChild(listEl);

  // An empty list is where the create action belongs: right under the sentence
  // asking for one, instead of orphaned at the top of an otherwise blank page.
  // Defaulted here so every view gets it from the createLabel/onCreate it
  // already supplies; a caller that sets its own action keeps it.
  let emptyOpts = empty;
  const plainObject = empty && typeof empty === 'object' && empty.nodeType !== 1;
  if (plainObject && !empty.action && canCreate) {
    emptyOpts = { ...empty, action: { label: createLabel, onClick: () => onCreate(() => list.reload()) } };
  }

  const list = createListView({
    scope,
    container: listEl,
    fetch,
    renderItem,
    empty: emptyOpts,
    errorMsg,
    filter,
  });

  if (toolReload) {
    watchToolReload(scope, projectId, toolReload, () => list.reload({ quiet: true }));
  }

  list.reload();

  return {
    reload: () => list.reload(),
    dispose: () => scope.dispose(),
    scope,
    body,
    list,
  };
}

/**
 * Shared danger-delete confirm: modal.confirmDanger + runAction with only the
 * labels/URL swapped per view. Pass the mutation itself as `run`; `success`
 * and `onDone` pass through to runAction.
 */
export function confirmDelete({ title, message, run, success, onDone }) {
  modal.confirmDanger(title, message, () => runAction(run, { success, onDone }));
}
