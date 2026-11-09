/**
 * App shell chrome: one thin bar across the top.
 *
 * It replaces the old sidebar. The sidebar rebuilt its whole nav on every
 * navigation AND on every pending-question event, which was affordable when it
 * owned nothing stateful. The top bar hosts the project switcher, so it isn't:
 * update() only toggles active classes and badge text, and the switcher element
 * is created once and never replaced. Rebuilding it would close an open
 * dropdown mid-search every time a question landed.
 */

import { h, clear } from '../core/dom.js';
import { t } from '../core/i18n.js';
import { navigate, currentPath } from '../core/router.js';
import { createScope } from '../core/lifecycle.js';
import { totalPending, watchPending } from '../core/questions.js';
import * as state from '../core/state.js';
import { icon } from './icon.js';
import { createThemeToggle } from './theme-toggle.js';
import { createProjectSwitcher } from './project-switcher.js';
import { showAboutModal } from './about-dialog.js';
import { showAccountModal } from './account-dialog.js';
import { logout } from './session.js';

// Everything that isn't a project. Lives behind one menu because none of it is
// touched often: the point of the revamp is that the prompt is the app.
// The slot the mounted project's controls live in. Module-level for the same
// reason setPanelSwitcher is: the bar is built once at boot and the project
// view has no reference to it.
let contextSlot = null;

/**
 * Put the mounted project's status and controls in the bar, or clear them.
 * The project's name and running dot are already the switcher's job, so this
 * is only what the switcher does not say.
 * @param {HTMLElement|null} element
 */
export function setProjectContext(element) {
  if (!contextSlot) return;
  clear(contextSlot);
  if (element) contextSlot.appendChild(element);
}

/**
 * Everything that isn't a project, in menu order. Exported because the command
 * palette offers the same destinations, and a second hand-kept copy of this
 * list would drift the first time a route is added.
 */
export const MENU_LINKS = [
  { path: '/projects', icon: 'globe', label: t('nav.allProjects') },
  { section: t('nav.providers') },
  { path: '/providers/llm', icon: 'brain', label: t('nav.llm') },
  { path: '/providers/image', icon: 'sparkle', label: t('nav.image') },
  { path: '/providers/stock', icon: 'image', label: t('nav.stock') },
  { path: '/providers/search', icon: 'search', label: t('nav.search') },
  { section: t('nav.system') },
  { path: '/messaging-apps', icon: 'chat', label: t('nav.messaging') },
  { path: '/service-providers', icon: 'shield', label: t('nav.services') },
  { path: '/questions', icon: 'help-circle', label: t('nav.questions'), badge: true },
  { path: '/users', icon: 'user', label: t('nav.users') },
  { path: '/usage', icon: 'activity', label: t('nav.usage') },
  { path: '/audit', icon: 'list', label: t('nav.audit') },
  { path: '/settings', icon: 'settings', label: t('nav.settings') },
];

/**
 * Create the top bar. One instance per app boot; it outlives every view.
 * @returns {{ element: HTMLElement, update: Function, openGlobalMenu: Function,
 *   closeMenus: Function, destroy: Function }}
 */
export function createTopBar() {
  const scope = createScope();
  const switcher = createProjectSwitcher();

  const themeBtn = createThemeToggle(scope);

  // --- Global menu popover ---
  const menuBadge = h('span', { className: 'nav-badge nav-badge--pulse', style: { display: 'none' } });
  const menuBtn = h('button', {
    className: 'btn btn--ghost btn--sm topbar__menu-btn',
    title: t('nav.menu'),
    'aria-label': t('nav.menu'),
    'aria-expanded': 'false',
    // detail 0 means the click came from the keyboard (Enter/Space on the button).
    onClick: (e) => { e.stopPropagation(); toggleMenu(e.detail === 0); },
  }, [
    h('span', { innerHTML: icon('menu') }),
    menuBadge,
  ]);

  const menu = h('div', { className: 'topbar__menu' });
  let menuOpen = false;

  function renderMenu() {
    clear(menu);
    const current = currentPath();
    for (const link of MENU_LINKS) {
      if (link.section) {
        menu.appendChild(h('div', { className: 'topbar__menu-label' }, link.section));
        continue;
      }
      const children = [
        h('span', { innerHTML: icon(link.icon) }),
        h('span', {}, link.label),
      ];
      if (link.badge) {
        const count = totalPending();
        if (count > 0) children.push(h('span', { className: 'nav-badge nav-badge--pulse' }, String(count)));
      }
      // A real link: focusable, Enter follows it, and "open in new tab" works.
      // data-route lets the router intercept the click for in-app navigation.
      menu.appendChild(h('a', {
        className: `topbar__menu-item${current === link.path ? ' active' : ''}`,
        href: link.path,
        'data-route': '',
        'aria-current': current === link.path ? 'page' : null,
        onClick: () => closeMenus(false),
      }, children));
    }

    // The bar keeps only the theme toggle, so everything else ends up here.
    // Sign out is last: it is the one item you never want to hit by accident.
    menu.appendChild(h('div', { className: 'topbar__menu-actions' }, [
      h('button', {
        className: 'topbar__menu-item',
        type: 'button',
        onClick: () => { closeMenus(false); showAccountModal(); },
      }, [
        h('span', { innerHTML: icon('user') }),
        h('span', {}, t('account.title')),
      ]),
      h('button', {
        className: 'topbar__menu-item',
        type: 'button',
        onClick: () => { closeMenus(false); showAboutModal(); },
      }, [
        h('span', { innerHTML: icon('grid-dots') }),
        h('span', {}, t('nav.about')),
      ]),
      h('button', {
        className: 'topbar__menu-item',
        type: 'button',
        onClick: () => { closeMenus(false); logout(); },
      }, [
        h('span', { innerHTML: icon('log-out') }),
        h('span', {}, t('nav.signOut')),
      ]),
    ]));
  }

  // Outside-click dismissal, same shape the project switcher uses.
  function handleOutsideClick(e) {
    if (!menu.contains(e.target) && !menuBtn.contains(e.target)) closeMenus();
  }
  function handleEscape(e) {
    if (e.key === 'Escape') closeMenus();
  }

  // Opening the project switcher closes this menu (and the reverse is in the
  // switcher); both stop click propagation, so neither would otherwise notice.
  scope.on(document, 'of:popover-open', (e) => {
    if (menuOpen && e.detail !== 'menu') closeMenus(false);
  });

  function openGlobalMenu(focusFirst = false) {
    if (menuOpen) return;
    menuOpen = true;
    renderMenu();
    menu.classList.add('visible');
    menuBtn.setAttribute('aria-expanded', 'true');
    document.dispatchEvent(new CustomEvent('of:popover-open', { detail: 'menu' }));
    // Opened from the keyboard: move into the menu so Tab and Enter work there.
    if (focusFirst === true) requestAnimationFrame(() => { const first = menu.querySelector('a, button'); if (first) first.focus(); });
    setTimeout(() => {
      document.addEventListener('click', handleOutsideClick);
      document.addEventListener('keydown', handleEscape);
    }, 0);
  }

  // `restoreFocus`: put focus back on the menu button, so a keyboard user who
  // pressed Escape is not dropped at the top of the page. Off when the menu
  // closes because something else took focus on purpose (a dialog, a link).
  function closeMenus(restoreFocus = true) {
    if (!menuOpen) return;
    const hadFocus = menu.contains(document.activeElement);
    menuOpen = false;
    menu.classList.remove('visible');
    menuBtn.setAttribute('aria-expanded', 'false');
    document.removeEventListener('click', handleOutsideClick);
    document.removeEventListener('keydown', handleEscape);
    if (restoreFocus && hadFocus) menuBtn.focus();
  }
  scope.add(() => closeMenus(false));

  function toggleMenu(fromKeyboard = false) {
    if (menuOpen) closeMenus(); else openGlobalMenu(fromKeyboard);
  }

  contextSlot = h('div', { className: 'topbar__context' });

  const element = h('header', { className: 'topbar' }, [
    h('div', { className: 'topbar__switcher' }, [switcher.element]),
    // Always visible rather than buried in the switcher dropdown. Goes to the
    // composer, not the project list: the prompt is how you make a project.
    h('button', {
      className: 'btn btn--ghost btn--sm topbar__new',
      title: t('nav.newProject'),
      onClick: () => navigate('/new'),
    }, [
      h('span', { innerHTML: icon('plus') }),
      h('span', {}, t('nav.newProject')),
    ]),
    contextSlot,
    h('div', { className: 'topbar__actions' }, [
      // Stays visible after the shell banner is dismissed. Opens About, which
      // says what the early release status means.
      h('button', {
        className: 'preview-badge',
        type: 'button',
        title: t('nav.earlyReleaseTip'),
        onClick: () => showAboutModal(),
      }, t('preview.title')),
      themeBtn,
      h('div', { className: 'topbar__menu-wrap' }, [menuBtn, menu]),
    ]),
  ]);

  function updateBadge() {
    const count = totalPending();
    menuBadge.textContent = String(count);
    menuBadge.style.display = count > 0 ? '' : 'none';
    if (menuOpen) renderMenu();
  }

  /** Active state and badges only. Never rebuilds the switcher. */
  function update() {
    switcher.update();
    updateBadge();
    if (menuOpen) renderMenu();
  }

  scope.on(window, 'routechange', update);
  state.select('projects', () => switcher.update(), scope);
  state.select('projectActivity', () => switcher.update(), scope);
  watchPending(updateBadge, scope);

  update();

  return {
    element,
    update,
    openGlobalMenu,
    closeMenus,
    destroy: () => scope.dispose(),
  };
}
