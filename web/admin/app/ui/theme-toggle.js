/**
 * The light/dark toggle button, shared by the signed-in top bar and the
 * sign-in / setup bar so both look and behave the same.
 */

import { h } from '../core/dom.js';
import * as theme from '../core/theme.js';
import { t } from '../core/i18n.js';
import { icon } from './icon.js';

/**
 * Create the toggle. It follows the theme rather than assuming it was the one
 * that changed it: the command palette can toggle the theme too.
 * @param {{ on: Function }} [scope] lifecycle scope that owns the listener;
 *   without one the listener lives as long as the page, which is right for the
 *   pre-auth screens (they end in a full reload).
 * @returns {HTMLButtonElement}
 */
export function createThemeToggle(scope) {
  const btn = h('button', {
    className: 'btn btn--ghost btn--sm',
    type: 'button',
    onClick: () => { theme.toggle(); sync(); },
  });

  function sync() {
    const current = theme.get();
    const label = t(current === 'dark' ? 'theme.toLight' : 'theme.toDark');
    btn.innerHTML = icon(current === 'dark' ? 'sun' : 'moon');
    btn.title = label;
    btn.setAttribute('aria-label', label);
  }
  sync();

  if (scope) scope.on(window, 'themechange', sync);
  else window.addEventListener('themechange', sync);

  return btn;
}
