/**
 * Frame for the screens you see before the app shell exists: setup, login and
 * the connection error. Deliberately built from the signed-in chrome (the same
 * .topbar strip, a card with the modal's header/body/footer anatomy) so the
 * first screens already look like the app they lead into.
 */

import { h } from '../core/dom.js';
import { createThemeToggle } from './theme-toggle.js';
import { createLanguagePicker } from './language-picker.js';
import { TAGLINE } from './splash.js';

/**
 * Build the bar + centred content column.
 * @param {{ children?: Node[], tagline?: boolean, languagePicker?: boolean }} [opts]
 * @returns {{ root: HTMLElement, main: HTMLElement, brandMark: HTMLImageElement }}
 *   brandMark is the logo in the bar, the splash morph's landing spot.
 */
export function createAuthShell(opts = {}) {
  const { children = [], tagline = true, languagePicker = true } = opts;

  const brandMark = h('img', {
    src: '/openfabrica_logo.svg',
    className: 'brand-mark auth-bar__mark',
    alt: '',
  });

  const bar = h('header', { className: 'topbar topbar--auth' }, [
    h('div', { className: 'auth-bar__brand' }, [
      brandMark,
      h('span', { className: 'auth-bar__name' }, 'Open Fabrica'),
    ]),
    h('div', { className: 'topbar__actions' }, [...(languagePicker ? [createLanguagePicker({ compact: true })] : []), createThemeToggle()]),
  ]);

  const main = h('main', { className: 'auth-main' }, [
    ...children,
    // Same tribute line the splash and the About dialog quote.
    ...(tagline ? [h('p', { className: 'auth-tagline' }, TAGLINE)] : []),
  ]);

  const root = h('div', { className: 'auth-layout' }, [bar, main]);
  return { root, main, brandMark };
}

/**
 * A card with the modal anatomy. Pass tag 'form' to make Enter submit.
 * @param {{ tag?: string, compact?: boolean, title?: string, aside?: Node,
 *   header?: Node[], body?: Node[], footer?: Node[], className?: string }} opts
 * @returns {{ card: HTMLElement, title: HTMLElement, header: HTMLElement,
 *   body: HTMLElement, footer: HTMLElement }}
 */
export function authCard(opts = {}) {
  const {
    tag = 'div', compact = false, title = '', aside = null,
    header: headerExtra = [], body: bodyChildren = [], footer: footerChildren = [],
    className = '',
  } = opts;

  const titleEl = h('h2', { className: 'modal__title' }, title);
  const header = h('div', { className: 'modal__header' }, [titleEl, ...(aside ? [aside] : [])]);
  const body = h('div', { className: 'modal__body' }, bodyChildren);
  const footer = h('div', { className: 'modal__footer' }, footerChildren);

  const classes = ['auth-card'];
  if (compact) classes.push('auth-card--compact');
  if (className) classes.push(className);

  const card = h(tag, { className: classes.join(' ') }, [header, ...headerExtra, body, footer]);
  return { card, title: titleEl, header, body, footer };
}
