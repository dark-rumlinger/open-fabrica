/**
 * App shell layout: top bar + main content area + mobile bottom nav.
 */

import { h } from '../core/dom.js';
import { t } from '../core/i18n.js';
import { navigate, currentPath, getActiveProjectId } from '../core/router.js';
import { pendingFor, watchPending } from '../core/questions.js';
import * as state from '../core/state.js';
import { createTopBar, MENU_LINKS } from './topbar.js';
import { createPreviewBanner } from './preview-banner.js';
import { icon } from './icon.js';
import { setPageTitle } from './tab-indicator.js';
import { titleForPath } from './page-title.js';
import { GROUPS } from './project-tabs.js';

/**
 * Create the app layout and return references to the shell pieces.
 * `root` must stay the element the connection banner is appended to, and `main`
 * the scrolling content column.
 * @returns {{ root: HTMLElement, main: HTMLElement, topbar: object, backdrop: HTMLElement }}
 */
export function createLayout() {
  const topbar = createTopBar();

  // A real <main> landmark, focusable so the skip link (and a screen reader's
  // "jump to main") has somewhere to land.
  const main = h('main', { className: 'main-content', id: 'main-content', tabindex: '-1' });

  const bottomNav = createBottomNav(topbar);

  // First thing Tab reaches: lets a keyboard user skip the bar's controls.
  const skipLink = h('a', {
    className: 'skip-link',
    href: '#main-content',
    onClick: (e) => { e.preventDefault(); main.focus(); },
  }, t('nav.skip'));

  const previewBanner = createPreviewBanner();

  const root = h('div', { className: 'app-layout' }, [
    skipLink,
    topbar.element,
    previewBanner.element,
    main,
    bottomNav.element,
  ]);

  // Shadow under the top bar once the content column has moved, so the bar
  // reads as sitting above the page rather than flush with it. Only the main
  // column is watched: the project view scrolls its own inner panes, where the
  // tab bar is the thing content passes under, not this one.
  main.addEventListener('scroll', () => {
    root.classList.toggle('is-scrolled', main.scrollTop > 2);
  }, { passive: true });

  // Update bottom nav on route change
  window.addEventListener('routechange', () => bottomNav.update());
  watchPending(() => bottomNav.update());
  bottomNav.update();

  // Each route gets its own tab title, and a new page opens at the top rather
  // than wherever the previous (taller) one was scrolled to.
  const panelLabel = (panelId) => {
    if (panelId === 'chat') return t('nav.chat');
    for (const group of GROUPS) {
      const exact = group.panels.find((p) => p.id === panelId);
      if (exact) return exact.label;
    }
    const base = String(panelId).split('-')[0];
    for (const group of GROUPS) {
      const hit = group.panels.find((p) => p.id === base);
      if (hit) return hit.label;
    }
    return '';
  };
  const updateTitle = () => setPageTitle(titleForPath(currentPath(), {
    links: MENU_LINKS,
    projects: state.get('projects') || [],
    panelLabel,
  }));
  let lastPath = currentPath();
  window.addEventListener('routechange', () => {
    const path = currentPath();
    if (path !== lastPath) {
      lastPath = path;
      main.scrollTop = 0;
    }
    updateTitle();
  });
  // A project's name can arrive (or change) after the route did.
  state.watch('projects', updateTitle);
  updateTitle();

  return { root, main, topbar };
}

/**
 * Create the mobile bottom navigation bar. The project's own surfaces are
 * reached through its tab bar, so this only has to get you to a project.
 */
function createBottomNav(topbar) {
  const dashItem = createNavItem('home', t('nav.new'), () => navigate('/new'));
  const projItem = createNavItem('globe', t('nav.projects'), () => navigate('/projects'));
  const projectItem = createNavItem('chat', t('nav.project'), () => {
    const projectId = getActiveProjectId();
    if (projectId) {
      navigate(`/projects/${projectId}/chat`);
    } else {
      navigate('/projects');
    }
  });
  const moreItem = createNavItem('menu', t('nav.more'), () => topbar.openGlobalMenu());

  // Badge for project item (pending questions)
  const badge = h('span', { className: 'bottom-nav__badge', style: { display: 'none' } });
  projectItem.el.appendChild(badge);

  const items = h('div', { className: 'bottom-nav__items' }, [
    dashItem.el,
    projItem.el,
    projectItem.el,
    moreItem.el,
  ]);

  const element = h('nav', { className: 'bottom-nav', 'aria-label': t('nav.primary') }, [items]);

  function update() {
    const path = currentPath();
    const projectId = getActiveProjectId();

    // Highlight active item
    const setActive = (item, on) => {
      item.el.classList.toggle('active', on);
      item.el.setAttribute('aria-current', on ? 'page' : 'false');
    };
    setActive(dashItem, path === '/new');
    setActive(projItem, path === '/projects');
    setActive(projectItem, !!projectId && path !== '/new' && path !== '/projects');
    moreItem.el.classList.remove('active');

    if (projectId) {
      // Update badge
      const count = pendingFor(projectId);
      if (count > 0) {
        badge.textContent = String(count);
        badge.style.display = '';
      } else {
        badge.style.display = 'none';
      }
    } else {
      badge.style.display = 'none';
    }
  }

  return { element, update };
}

function createNavItem(iconName, label, onClick) {
  const el = h('button', {
    className: 'bottom-nav__item',
    type: 'button',
    onClick,
  }, [
    h('span', { innerHTML: icon(iconName) }),
    h('span', {}, label),
  ]);
  return { el };
}
