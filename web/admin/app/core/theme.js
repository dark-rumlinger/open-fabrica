/**
 * Theme toggle with localStorage persistence.
 *
 * With no saved choice the theme is dark. Picking a theme saves it. The stylesheet's :root
 * palette is the light one, so a document with no attribute renders light; the bootstrap
 * in theme-boot.js applies the saved or default theme before first paint.
 */

export const THEME_KEY = 'of_theme';

/**
 * Get the current theme.
 */
export function get() {
  return document.documentElement.getAttribute('data-theme') || 'light';
}

export const DEFAULT_THEME = 'dark';

function saved() {
  try {
    return localStorage.getItem(THEME_KEY);
  } catch { return null; /* storage off: no saved choice */ }
}

/** Apply a theme without saving it. */
function apply(theme) {
  document.documentElement.setAttribute('data-theme', theme);
  // The top bar's toggle is not the only way to change the theme (the
  // command palette does it too), so the change is announced rather than left
  // for whoever happened to call set() to repaint their own button. Guarded
  // because the unit tests stub document and storage but not window.
  if (typeof window !== 'undefined' && typeof window.dispatchEvent === 'function') {
    window.dispatchEvent(new CustomEvent('themechange', { detail: theme }));
  }
}

/**
 * Set the theme explicitly and remember the choice.
 */
export function set(theme) {
  try {
    localStorage.setItem(THEME_KEY, theme);
  } catch { /* storage off: the theme still applies, it just won't persist */ }
  apply(theme);
}

/**
 * Toggle between dark and light.
 */
export function toggle() {
  const current = get();
  const next = current === 'dark' ? 'light' : 'dark';
  set(next);
  return next;
}

/**
 * Initialize the theme: the saved choice, else the dark default. The default is applied
 * but not stored.
 */
export function init() {
  apply(saved() || DEFAULT_THEME);
}
