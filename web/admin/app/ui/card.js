/**
 * Shared card chrome, header (title + optional badge) and a row of action
 * buttons. Used by list-view items across users, secrets, webhooks, etc.
 */

import { h } from '../core/dom.js';
import { icon } from './icon.js';
import { isAdmin } from '../core/state.js';

// An action flagged adminOnly is dropped for non-admins: every write route is
// admin-only server-side, so showing the button only invites a 403.
const allowed = (a) => !a.hidden && !(a.adminOnly && !isAdmin());

/**
 * card({ title, subtitle?, iconName?, badge?, actions?: [{label, iconName?, kind?, onClick, hidden?, adminOnly?}], children? })
 */
export function card(opts) {
  const { title, subtitle, iconName, badge, actions = [], children = [] } = opts;

  const titleNode = h('div', {}, [
    h('h4', { className: 'card__title' }, title),
    ...(subtitle ? [h('span', { className: 'text-xs text-secondary' }, subtitle)] : []),
  ]);

  const left = iconName
    ? h('div', { className: 'flex items-center gap-3' }, [
        h('span', { innerHTML: icon(iconName) }),
        titleNode,
      ])
    : titleNode;

  const headerChildren = [left];
  if (badge) headerChildren.push(badge);

  const visibleActions = actions.filter(allowed);
  const actionRow = visibleActions.length
    ? h('div', { className: 'flex items-center gap-2 mt-4' },
        visibleActions.map(a => h('button', {
          className: a.className || `btn btn--${a.kind || 'ghost'} btn--sm`,
          onClick: a.onClick,
        }, [
          ...(a.iconName ? [h('span', { innerHTML: icon(a.iconName) })] : []),
          a.label,
        ])))
    : null;

  const cardChildren = [
    h('div', { className: 'card__header' }, headerChildren),
    ...(Array.isArray(children) ? children : [children]),
    ...(actionRow ? [actionRow] : []),
  ];

  return h('div', { className: 'card mb-4' }, cardChildren);
}

/**
 * pageHeader({ title, actions? }), standard context-panel header.
 *
 * No subtitle: every page carried a sentence explaining itself, which on the
 * project panels landed directly under a tab and a rail entry that had already
 * named the screen. Per-field help still lives in form hints, and empty states
 * still explain themselves - those arrive when they are useful rather than on
 * every visit.
 */
export function pageHeader(opts) {
  const { title, actions = [] } = opts;
  const visible = actions.filter(allowed);
  const right = visible.map(a => h('button', {
    className: a.className || `btn btn--${a.kind || 'primary'}`,
    onClick: a.onClick,
  }, [
    ...(a.iconName ? [h('span', { innerHTML: icon(a.iconName) })] : []),
    a.label,
  ]));

  const left = h('h3', { className: 'context-panel__title context-panel__title--page' }, title);

  return h('div', {
    className: 'context-panel__header context-panel__header--page flex items-center justify-between',
  }, [
    left,
    // Group the action buttons so justify-between only spreads the title vs.
    // the button cluster. Spreading the buttons individually would scatter
    // them across the bar (a lone middle button when there are two).
    h('div', { className: 'flex items-center gap-2' }, right),
  ]);
}

/** body container for the panel-style page bodies */
export function pageBody() {
  return h('div', { className: 'context-panel__body context-panel__body--page' });
}
