/**
 * Command palette. Ctrl/Cmd-K over what the top bar already knows: every
 * project, every route in the global menu, and the few actions that otherwise
 * cost two clicks through a popover.
 *
 * Mounted once per boot from main.js and never torn down, so it holds no view
 * state: the item list is rebuilt from `state` each time it opens rather than
 * cached, which is what keeps a project created in another tab from being
 * missing here.
 */

import { h, clear } from '../core/dom.js';
import { t } from '../core/i18n.js';
import { navigate, getActiveProjectId } from '../core/router.js';
import * as state from '../core/state.js';
import * as theme from '../core/theme.js';
import { icon } from './icon.js';
import { createPhaseDot } from './phase-badge.js';
import { MENU_LINKS } from './topbar.js';
import { GROUPS } from './project-tabs.js';
import { showAboutModal } from './about-dialog.js';
import { logout } from './session.js';

// Long lists stop being scannable well before this; the filter is the way to
// reach anything past it.
const MAX_RESULTS = 40;

let overlay = null;
let input = null;
let listEl = null;
let emptyEl = null;

let isOpen = false;
let rows = [];          // [{ item, el }] in rendered order
let activeIndex = 0;
let lastFocused = null;

/**
 * Everything the palette can do, grouped in the order the groups are shown.
 * Rebuilt on every open.
 */
function buildItems() {
  const items = [];

  // Jump to a part of the project you are in. Typing "pages" or "secrets" is
  // quicker than finding the tab, and the entry says which project it means.
  const projects = state.get('projects') || [];
  const activeId = getActiveProjectId();
  const active = projects.find((p) => p.id === activeId);
  if (active) {
    for (const group of GROUPS) {
      for (const panel of group.panels) {
        if (panel.adminOnly && !state.isAdmin()) continue;
        items.push({
          group: t('cmdk.thisProject'),
          label: panel.label,
          hint: active.name,
          iconName: 'chevron-right',
          run: () => navigate(`/projects/${active.id}/${panel.id}`),
        });
      }
    }
  }

  for (const project of projects) {
    items.push({
      group: t('nav.projects'),
      label: project.name,
      hint: project.domain || '',
      iconName: 'globe',
      phase: project.phase,
      run: () => navigate(`/projects/${project.id}/chat`),
    });
  }

  for (const link of MENU_LINKS) {
    if (link.section) continue;
    items.push({
      group: t('cmdk.goTo'),
      label: link.label,
      iconName: link.icon,
      run: () => navigate(link.path),
    });
  }

  items.push({
    group: t('cmdk.actions'),
    label: t('nav.newProject'),
    iconName: 'plus',
    run: () => navigate('/new'),
  });
  items.push({
    group: t('cmdk.actions'),
    label: t(theme.get() === 'dark' ? 'theme.toLight' : 'theme.toDark'),
    iconName: theme.get() === 'dark' ? 'sun' : 'moon',
    run: () => theme.toggle(),
  });
  items.push({
    group: t('cmdk.actions'),
    label: t('nav.about'),
    iconName: 'grid-dots',
    run: () => showAboutModal(),
  });
  items.push({
    group: t('cmdk.actions'),
    label: t('nav.signOut'),
    iconName: 'log-out',
    run: () => logout(),
  });

  return items;
}

/**
 * Rank one item against the search term. Higher is better, -1 drops it.
 * Deliberately not a fuzzy matcher: substring with a bonus for matching at a
 * word boundary puts "Users" above "AI Usage" for "us" without the jumpy
 * re-ordering a character-skipping score produces as you type.
 */
function score(item, term) {
  const hay = (item.label + ' ' + (item.hint || '')).toLowerCase();
  const at = hay.indexOf(term);
  if (at < 0) return -1;
  if (at === 0) return 3;
  return /[\s\-._/]/.test(hay[at - 1]) ? 2 : 1;
}

function filtered(term) {
  const items = buildItems();
  if (!term) return items.slice(0, MAX_RESULTS);

  const scored = [];
  for (const item of items) {
    const s = score(item, term);
    if (s >= 0) scored.push({ item, s });
  }
  // Stable sort, so equally-scored items keep their natural (grouped) order.
  scored.sort((a, b) => b.s - a.s);
  return scored.slice(0, MAX_RESULTS).map(entry => entry.item);
}

function createRow(item, index) {
  const children = [
    h('span', { className: 'cmdk__row-icon', innerHTML: icon(item.iconName) }),
    h('span', { className: 'cmdk__row-label' }, item.label),
  ];
  if (item.phase) {
    children.push(createPhaseDot(item.phase));
  }
  if (item.hint) {
    children.push(h('span', { className: 'cmdk__row-hint' }, item.hint));
  }

  return h('div', {
    className: 'cmdk__row',
    role: 'option',
    id: `cmdk-opt-${index}`,
    'aria-selected': 'false',
    // mousedown, not click: the input must not lose focus before the action.
    onMousedown: (e) => { e.preventDefault(); runItem(item); },
    onMousemove: () => setActive(index),
  }, children);
}

function render() {
  const term = input.value.trim().toLowerCase();
  const items = filtered(term);

  clear(listEl);
  rows = [];

  let lastGroup = null;
  items.forEach((item) => {
    // Headings only while browsing. Once a term ranks items across groups, a
    // heading no longer describes the rows under it.
    if (!term && item.group !== lastGroup) {
      lastGroup = item.group;
      listEl.appendChild(h('div', { className: 'cmdk__group' }, item.group));
    }
    const el = createRow(item, rows.length);
    rows.push({ item, el });
    listEl.appendChild(el);
  });

  emptyEl.hidden = items.length > 0;
  listEl.hidden = items.length === 0;
  setActive(0);
}

function setActive(index) {
  if (!rows.length) {
    activeIndex = 0;
    input.removeAttribute('aria-activedescendant');
    return;
  }
  activeIndex = Math.max(0, Math.min(index, rows.length - 1));
  rows.forEach((row, i) => {
    const on = i === activeIndex;
    row.el.classList.toggle('is-active', on);
    row.el.setAttribute('aria-selected', on ? 'true' : 'false');
  });
  const active = rows[activeIndex].el;
  input.setAttribute('aria-activedescendant', active.id);
  active.scrollIntoView({ block: 'nearest' });
}

function move(delta) {
  if (!rows.length) return;
  // Wraps, so Up from the first row reaches the last without a long hold.
  const next = (activeIndex + delta + rows.length) % rows.length;
  setActive(next);
}

function runItem(item) {
  close();
  item.run();
}

function onKeydown(e) {
  if (e.isComposing) return;
  switch (e.key) {
    case 'Escape':
      e.preventDefault();
      e.stopPropagation();
      close();
      break;
    case 'ArrowDown':
      e.preventDefault();
      move(1);
      break;
    case 'ArrowUp':
      e.preventDefault();
      move(-1);
      break;
    case 'Home':
      e.preventDefault();
      setActive(0);
      break;
    case 'End':
      e.preventDefault();
      setActive(rows.length - 1);
      break;
    case 'Enter':
      e.preventDefault();
      if (rows[activeIndex]) runItem(rows[activeIndex].item);
      break;
    case 'Tab':
      // Nothing else in here is focusable, so the trap is just staying put.
      e.preventDefault();
      break;
    default:
      break;
  }
}

function build() {
  input = h('input', {
    className: 'cmdk__input',
    type: 'text',
    id: 'cmdk-input',
    placeholder: t('cmdk.placeholder'),
    autocomplete: 'off',
    spellcheck: 'false',
    role: 'combobox',
    'aria-expanded': 'true',
    'aria-controls': 'cmdk-list',
    'aria-autocomplete': 'list',
    onInput: render,
    onKeydown,
  });

  listEl = h('div', { className: 'cmdk__list', id: 'cmdk-list', role: 'listbox' });
  emptyEl = h('div', { className: 'cmdk__empty', hidden: true }, t('cmdk.noMatches'));

  const panel = h('div', {
    className: 'cmdk',
    role: 'dialog',
    'aria-modal': 'true',
    'aria-label': t('cmdk.label'),
    onMousedown: (e) => e.stopPropagation(),
  }, [
    h('div', { className: 'cmdk__search' }, [
      h('span', { className: 'cmdk__search-icon', innerHTML: icon('search') }),
      input,
      h('kbd', { className: 'cmdk__kbd' }, 'Esc'),
    ]),
    listEl,
    emptyEl,
    h('div', { className: 'cmdk__footer' }, [
      h('span', {}, [h('kbd', { className: 'cmdk__kbd' }, '↑↓'), ' ' + t('cmdk.navigate')]),
      h('span', {}, [h('kbd', { className: 'cmdk__kbd' }, '↵'), ' ' + t('cmdk.open')]),
    ]),
  ]);

  overlay = h('div', {
    className: 'cmdk-overlay',
    onMousedown: () => close(),
  }, [panel]);

  document.body.appendChild(overlay);
}

/** Open the palette, or do nothing if it is already up. */
function open() {
  if (isOpen) return;
  if (!overlay) build();

  isOpen = true;
  lastFocused = document.activeElement;
  input.value = '';
  overlay.classList.add('is-open');
  render();
  input.focus();
}

/** Close the palette and hand focus back to whatever had it. */
function close() {
  if (!isOpen) return;
  isOpen = false;
  overlay.classList.remove('is-open');
  clear(listEl);
  rows = [];
  if (lastFocused && typeof lastFocused.focus === 'function') {
    try { lastFocused.focus(); } catch { /* element may be gone with its view */ }
  }
  lastFocused = null;
}

/**
 * Install the global shortcut. Called once from boot; there is no matching
 * unmount because the palette lives as long as the app shell does.
 */
export function mountCommandPalette() {
  document.addEventListener('keydown', (e) => {
    // e.code is the physical key, so the shortcut also works on layouts where
    // that key types another letter.
    if (e.code !== 'KeyK') return;
    if (!e.metaKey && !e.ctrlKey) return;
    // Ctrl-Shift-K and friends belong to the browser.
    if (e.altKey || e.shiftKey) return;
    e.preventDefault();
    if (isOpen) close(); else open();
  });
}
