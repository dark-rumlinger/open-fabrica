/**
 * Sectioned-panel scaffold: pageHeader + one visible section.
 *
 * Sections are pills in the project's own tab bar, not a `.tabs` bar drawn inside
 * the panel, so there is no extra strip of chrome and no duplicate "General". The
 * active section arrives from the route, and this draws no bar of its own.
 *
 * Content is still built eagerly by the caller for every section, exactly as
 * before - only the active one is displayed.
 *
 * Opts: { container, title, activeSection?, defaultTab?,
 *         tabs: [{ id, label, content }] }.
 */

import { h } from '../core/dom.js';
import { pageHeader } from './card.js';

export function createTabbedPanel(opts) {
  const { container, title, tabs, defaultTab, activeSection } = opts;

  if (!container) throw new Error('createTabbedPanel: container required');
  if (!Array.isArray(tabs) || tabs.length === 0) throw new Error('createTabbedPanel: tabs required');

  container.replaceChildren();

  const ids = tabs.map((t) => t.id);

  // An unknown section (a hand-typed URL, or one hidden from this user) falls
  // back rather than rendering a panel with nothing in it.
  const activeTab = ids.includes(activeSection) ? activeSection
    : (defaultTab && ids.includes(defaultTab)) ? defaultTab
    : ids[0];

  const header = pageHeader({ title });

  const bodyChildren = [];
  for (const t of tabs) {
    t.content.style.display = t.id === activeTab ? '' : 'none';
    bodyChildren.push(t.content);
  }

  const body = h('div', { className: 'context-panel__body' }, bodyChildren);
  container.appendChild(header);
  container.appendChild(body);

  return { header, body, activeTab: () => activeTab };
}
