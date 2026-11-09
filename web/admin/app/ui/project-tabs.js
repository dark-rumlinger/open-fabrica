/**
 * The project's navigation: one row of tabs, and a rail down the side listing
 * whatever is inside the active tab.
 *
 * The sections were a second horizontal row of pills, which meant two bars in
 * two visual languages stacked above every panel. Vertically they cost no
 * height, they scale past six without scrolling sideways, and they sit next to
 * the thing they change.
 *
 * It only navigates - the route stays the single source of truth for which
 * panel is showing, so deep links and the chat cards' panel switcher keep
 * working untouched.
 */

import { h, clear } from '../core/dom.js';
import { t } from '../core/i18n.js';
import { navigate } from '../core/router.js';
import { pendingFor, watchPending } from '../core/questions.js';
import * as state from '../core/state.js';

/**
 * Tab -> panels. The first panel in a group is where its tab lands you.
 *
 * Panel ids are the route's :tab segment. An id with a dash names a section
 * inside a panel that has several ("settings-advanced" is the settings panel
 * showing its Advanced section), so those sections share this one tab bar rather
 * than drawing a second row of navigation with its own duplicate "General".
 * splitPanelId below is what views/project/index.js uses to route them.
 */
export const GROUPS = [
  { id: 'chat', label: t('nav.chat'), panels: [{ id: 'chat', label: t('nav.chat') }] },
  {
    id: 'app',
    label: t('tabs.app'),
    panels: [
      { id: 'pages', label: t('tabs.pages') },
      { id: 'layouts', label: t('tabs.layouts') },
      { id: 'layouts-components', label: t('tabs.components') },
      { id: 'files', label: t('tabs.files') },
    ],
  },
  {
    id: 'data',
    label: t('tabs.data'),
    panels: [
      { id: 'tables', label: t('tabs.tables') },
      { id: 'endpoints', label: t('tabs.apis') },
      { id: 'webhooks', label: t('tabs.eventHooks') },
      { id: 'webhooks-actions', label: t('tabs.automated') },
      { id: 'tasks', label: t('tabs.scheduled') },
      // Orders and subscriptions carry buyer references, so the owner alone sees them.
      { id: 'payments', label: t('tabs.payments'), adminOnly: true },
    ],
  },
  {
    id: 'activity',
    label: t('tabs.activity'),
    panels: [
      { id: 'builds', label: t('tabs.builds') },
      { id: 'tasks-background', label: t('tabs.background') },
      { id: 'analytics', label: t('tabs.analytics') },
      { id: 'questions', label: t('nav.questions'), badge: true },
      { id: 'diagnostics', label: t('tabs.health') },
    ],
  },
  {
    id: 'settings',
    label: t('tabs.settings'),
    panels: [
      { id: 'settings', label: t('tabs.general') },
      // The settings panel hides this section from non-admins, so the pill
      // that leads to it has to disappear with it.
      { id: 'settings-sharing', label: t('tabs.sharing'), adminOnly: true },
      { id: 'settings-advanced', label: t('tabs.advanced') },
      { id: 'secrets', label: t('tabs.apiKeys') },
      { id: 'providers', label: t('nav.services') },
      { id: 'memory', label: t('tabs.memory') },
    ],
  },
];

/**
 * Split a route's panel id into the panel that renders it and the section it
 * should open on. "settings-advanced" -> { panel: 'settings', section:
 * 'advanced' }; "pages" -> { panel: 'pages', section: null }. No base panel id
 * contains a dash, so the first one is always the separator.
 * @param {string} panelId
 */
export function splitPanelId(panelId) {
  const i = String(panelId || '').indexOf('-');
  if (i === -1) return { panel: panelId, section: null };
  return { panel: panelId.slice(0, i), section: panelId.slice(i + 1) };
}

/**
 * The group a panel belongs to. Falls back to the first group so an unknown
 * panel still renders a coherent bar rather than an empty one.
 * @param {string} panelId
 */
export function groupForPanel(panelId) {
  return GROUPS.find(g => g.panels.some(p => p.id === panelId)) || GROUPS[0];
}

/** The panels of a group this user can actually reach. */
function visiblePanels(group) {
  return group.panels.filter(p => !p.adminOnly || state.isAdmin());
}

/**
 * Build the tab bar for a project.
 * @param {{projectId: number, activePanel: string, scope: object}} opts
 * @returns {{ element: HTMLElement }}
 */
export function createProjectTabs({ projectId, activePanel, scope }) {
  const activeGroup = groupForPanel(activePanel);

  const tabRow = h('div', { className: 'project-tabs__row' });
  const element = h('nav', { className: 'project-tabs', 'aria-label': t('tabs.areas') }, [tabRow]);
  const rail = h('nav', { className: 'project-rail', 'aria-label': t('tabs.sections', { group: activeGroup.label }) });

  function go(panelId) {
    navigate(`/projects/${projectId}/${panelId}`);
  }

  function renderTabs() {
    clear(tabRow);
    for (const group of GROUPS) {
      const isActive = group.id === activeGroup.id;
      const landing = visiblePanels(group)[0] || group.panels[0];
      // A pending question is the one thing in here that needs answering, so it
      // is surfaced on the tab as well as the pill inside it.
      const wantsBadge = group.panels.some(p => p.badge) && pendingFor(projectId) > 0;

      const children = [h('span', {}, group.label)];
      if (wantsBadge) children.push(h('span', { className: 'nav-badge nav-badge--pulse' }, String(pendingFor(projectId))));

      tabRow.appendChild(h('button', {
        className: `project-tabs__tab${isActive ? ' is-active' : ''}`,
        'aria-current': isActive ? 'page' : null,
        onClick: () => go(landing.id),
      }, children));
    }
  }

  function renderRail() {
    clear(rail);
    for (const panel of visiblePanels(activeGroup)) {
      const children = [h('span', {}, panel.label)];
      if (panel.badge) {
        const count = pendingFor(projectId);
        if (count > 0) children.push(h('span', { className: 'nav-badge nav-badge--pulse' }, String(count)));
      }
      rail.appendChild(h('button', {
        className: `project-rail__item${panel.id === activePanel ? ' is-active' : ''}`,
        'aria-current': panel.id === activePanel ? 'page' : null,
        onClick: () => go(panel.id),
      }, children));
    }
  }

  function render() {
    renderTabs();
    renderRail();
  }

  // Only the badge counts change without a re-mount; the route change that
  // moves the active tab tears this whole bar down and builds a new one.
  watchPending(render, scope);
  render();

  // On a narrow screen the tab row and the rail scroll sideways, and every route
  // change builds them afresh at scroll 0, which can leave the current tab off
  // screen. Bring it back into view once the bar is in the page.
  requestAnimationFrame(() => {
    for (const strip of [tabRow, rail]) {
      const current = strip.querySelector('.is-active');
      if (current && typeof current.scrollIntoView === 'function') {
        current.scrollIntoView({ inline: 'center', block: 'nearest' });
      }
    }
  });

  // A tab with one panel (the chat) has nothing to list, so it gets no rail
  // and the panel takes the full width.
  return { element, rail: visiblePanels(activeGroup).length > 1 ? rail : null };
}
