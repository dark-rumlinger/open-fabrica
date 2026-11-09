/**
 * Project switcher component, searchable dropdown with favorites, recents, and all projects.
 *
 * Built as a combobox: the search field keeps focus the whole time and the
 * arrow keys move a highlight through the rows (aria-activedescendant), Enter
 * opens the highlighted project, Escape closes. The search field is created
 * once; only the list under it is rebuilt as you type, so the caret, IME
 * composition and fast typing are never disturbed.
 */

import { h, clear } from '../core/dom.js';
import { t } from '../core/i18n.js';
import { icon } from './icon.js';
import { navigate, getActiveProjectId } from '../core/router.js';
import * as state from '../core/state.js';
import { createPhaseDot } from './phase-badge.js';

const FAV_KEY = 'of_fav_projects';
const RECENT_KEY = 'of_recent_projects';
// Last project opened. Read by nothing in this module any more: it is kept
// for setActiveProject's callers (main.js tracks it as you navigate).
const ACTIVE_KEY = 'of_active_project';
const MAX_RECENTS = 5;
const MAX_FAVORITES = 10;

// Which popover is open, so opening one closes the others. The top-bar menu and
// this switcher both stop click propagation on their toggles, which used to
// keep each from ever seeing a click that should have closed the other.
const POPOVER_EVENT = 'of:popover-open';

// --- localStorage helpers (storage can be blocked; the switcher still works) ---

function readList(key) {
  try {
    const parsed = JSON.parse(localStorage.getItem(key));
    return Array.isArray(parsed) ? parsed : [];
  } catch { return []; }
}

function writeList(key, ids) {
  try { localStorage.setItem(key, JSON.stringify(ids)); } catch { /* storage blocked */ }
}

function getFavorites() { return readList(FAV_KEY); }

function toggleFavorite(projectId) {
  const favs = getFavorites();
  const idx = favs.indexOf(projectId);
  if (idx >= 0) {
    favs.splice(idx, 1);
  } else {
    // Over the cap the oldest favourite is dropped; the caller says so.
    favs.push(projectId);
    if (favs.length > MAX_FAVORITES) favs.shift();
  }
  writeList(FAV_KEY, favs);
  return favs;
}

function getRecents() { return readList(RECENT_KEY); }

export function addRecent(projectId) {
  let recents = getRecents();
  recents = recents.filter(id => id !== projectId);
  recents.unshift(projectId);
  if (recents.length > MAX_RECENTS) recents.length = MAX_RECENTS;
  writeList(RECENT_KEY, recents);
}

export function setActiveProject(projectId) {
  try {
    if (projectId != null) {
      localStorage.setItem(ACTIVE_KEY, String(projectId));
    } else {
      localStorage.removeItem(ACTIVE_KEY);
    }
  } catch { /* storage blocked */ }
}

let switcherSeq = 0;

/**
 * Create the project switcher component.
 * @returns {{ element: HTMLElement, update: () => void }}
 */
export function createProjectSwitcher() {
  const uid = ++switcherSeq;
  const listId = `project-switcher-list-${uid}`;
  let isOpen = false;
  let searchTerm = '';
  // Rows currently shown, in order, so the arrow keys can walk them.
  let rows = [];
  let activeIndex = -1;

  const btn = h('button', {
    className: 'project-switcher__btn',
    type: 'button',
    'aria-haspopup': 'listbox',
    'aria-expanded': false,
    'aria-controls': listId,
  });
  const dropdown = h('div', { className: 'project-switcher__dropdown' });

  const searchInput = h('input', {
    className: 'project-switcher__search',
    type: 'text',
    placeholder: t('switcher.search'),
    autocomplete: 'off',
    spellcheck: 'false',
    role: 'combobox',
    'aria-label': t('switcher.searchLabel'),
    'aria-expanded': true,
    'aria-controls': listId,
    'aria-autocomplete': 'list',
  });
  const listEl = h('div', { className: 'project-switcher__list', id: listId, role: 'listbox', 'aria-label': t('nav.projects') });
  const footerBtn = h('button', {
    className: 'project-switcher__footer-btn',
    type: 'button',
    onClick: (e) => {
      e.stopPropagation();
      close();
      navigate('/projects');
    },
  }, [
    h('span', { innerHTML: icon('globe') }),
    t('nav.allProjects'),
  ]);
  dropdown.append(searchInput, listEl, footerBtn);

  const element = h('div', { className: 'project-switcher' }, [btn, dropdown]);

  function renderButton() {
    clear(btn);
    // Only the route decides what the button shows. Falling back to the last
    // project in localStorage would make signing in look like you had already
    // opened one, on a screen whose whole point is starting a new one.
    const activeProjectId = getActiveProjectId();
    const projects = state.get('projects') || [];
    const project = projects.find(s => s.id === activeProjectId);

    if (project) {
      btn.appendChild(createPhaseDot(project.phase));
      btn.appendChild(h('span', { className: 'project-switcher__btn-name' }, project.name));
    } else {
      // The brand mark rather than a generic globe: with nothing selected this
      // is the only glyph in the bar besides the logo itself, and two different
      // marks side by side read as two different things.
      btn.appendChild(h('img', {
        src: '/openfabrica_logo.svg',
        className: 'brand-mark project-switcher__btn-mark',
        alt: '',
      }));
      btn.appendChild(h('span', { className: 'project-switcher__btn-name' }, t('switcher.select')));
    }

    btn.appendChild(h('span', {
      innerHTML: icon('chevron-down'),
      className: 'project-switcher__chevron',
    }));
  }

  function setActive(index) {
    rows.forEach((r, i) => {
      const on = i === index;
      r.el.classList.toggle('is-active', on);
      r.el.setAttribute('aria-selected', String(on));
    });
    activeIndex = index;
    if (index >= 0 && rows[index]) {
      searchInput.setAttribute('aria-activedescendant', rows[index].el.id);
      rows[index].el.scrollIntoView({ block: 'nearest' });
    } else {
      searchInput.removeAttribute('aria-activedescendant');
    }
  }

  function openProject(project) {
    addRecent(project.id);
    setActiveProject(project.id);
    close();
    navigate(`/projects/${project.id}/chat`);
  }

  function renderList() {
    clear(listEl);
    rows = [];
    const projects = state.get('projects') || [];
    const favIds = getFavorites();
    const recentIds = getRecents();
    const term = searchTerm.trim().toLowerCase();

    const filtered = term
      ? projects.filter(s =>
          s.name.toLowerCase().includes(term) ||
          (s.domain && s.domain.toLowerCase().includes(term)))
      : projects;

    const addRow = (project) => {
      const el = createProjectRow(project, favIds, rows.length);
      rows.push({ project, el });
      listEl.appendChild(el);
    };

    if (!term) {
      const favProjects = favIds.map(id => projects.find(s => s.id === id)).filter(Boolean);
      if (favProjects.length > 0) {
        listEl.appendChild(h('div', { className: 'project-switcher__section-label', role: 'presentation' }, t('switcher.favorites')));
        favProjects.forEach(addRow);
      }

      const recentProjects = recentIds
        .filter(id => !favIds.includes(id))
        .map(id => projects.find(s => s.id === id))
        .filter(Boolean);
      if (recentProjects.length > 0) {
        listEl.appendChild(h('div', { className: 'project-switcher__section-label', role: 'presentation' }, t('switcher.recent')));
        recentProjects.forEach(addRow);
      }

      if (projects.length > 0) {
        listEl.appendChild(h('div', { className: 'project-switcher__section-label', role: 'presentation' }, t('nav.allProjects')));
      }
    }

    filtered.forEach(addRow);

    if (filtered.length === 0) {
      listEl.appendChild(h('div', { className: 'project-switcher__empty', role: 'presentation' }, t('switcher.none')));
    }

    // With a search typed, pre-highlight the first match so Enter opens it.
    setActive(term && rows.length ? 0 : -1);
  }

  function createProjectRow(project, favIds, index) {
    const isFav = favIds.includes(project.id);

    return h('div', {
      className: 'project-switcher__row',
      id: `${listId}-opt-${index}`,
      role: 'option',
      'aria-selected': false,
      onClick: () => openProject(project),
      onMouseenter: () => setActive(index),
    }, [
      createPhaseDot(project.phase),
      h('span', { className: 'project-switcher__row-name' }, project.name),
      project.domain
        ? h('span', { className: 'project-switcher__row-domain' }, project.domain)
        : null,
      h('button', {
        className: `project-switcher__star${isFav ? ' active' : ''}`,
        type: 'button',
        title: isFav ? t('switcher.removeFav') : t('switcher.addFav'),
        'aria-label': t(isFav ? 'switcher.removeFavNamed' : 'switcher.addFavNamed', { name: project.name }),
        'aria-pressed': isFav,
        tabIndex: -1,
        innerHTML: icon(isFav ? 'star-filled' : 'star'),
        onClick: (e) => {
          e.stopPropagation();
          toggleFavorite(project.id);
          renderList();
        },
      }),
    ]);
  }

  searchInput.addEventListener('input', () => {
    searchTerm = searchInput.value;
    renderList();
  });

  searchInput.addEventListener('keydown', (e) => {
    if (e.isComposing) return;
    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        if (rows.length) setActive((activeIndex + 1) % rows.length);
        break;
      case 'ArrowUp':
        e.preventDefault();
        if (rows.length) setActive(activeIndex <= 0 ? rows.length - 1 : activeIndex - 1);
        break;
      case 'Home':
        if (rows.length) { e.preventDefault(); setActive(0); }
        break;
      case 'End':
        if (rows.length) { e.preventDefault(); setActive(rows.length - 1); }
        break;
      case 'Enter': {
        e.preventDefault();
        const pick = rows[activeIndex] || rows[0];
        if (pick) openProject(pick.project);
        break;
      }
      case 'Escape':
        e.stopPropagation();
        close();
        btn.focus();
        break;
      default:
    }
  });

  function open() {
    isOpen = true;
    searchTerm = '';
    searchInput.value = '';
    dropdown.classList.add('visible');
    element.classList.add('open');
    btn.setAttribute('aria-expanded', 'true');
    document.dispatchEvent(new CustomEvent(POPOVER_EVENT, { detail: 'switcher' }));
    renderList();
    // Click outside to close
    setTimeout(() => document.addEventListener('click', handleOutsideClick), 0);
    requestAnimationFrame(() => searchInput.focus());
  }

  function close() {
    isOpen = false;
    dropdown.classList.remove('visible');
    element.classList.remove('open');
    btn.setAttribute('aria-expanded', 'false');
    document.removeEventListener('click', handleOutsideClick);
  }

  function handleOutsideClick(e) {
    if (!element.contains(e.target)) {
      close();
    }
  }

  // Another popover (the top-bar menu) opened: step aside.
  document.addEventListener(POPOVER_EVENT, (e) => {
    if (isOpen && e.detail !== 'switcher') close();
  });

  btn.addEventListener('click', (e) => {
    e.stopPropagation();
    if (isOpen) {
      close();
    } else {
      open();
    }
  });

  function update() {
    renderButton();
  }

  // Initial render
  renderButton();

  return { element, update };
}
