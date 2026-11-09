/**
 * Dismissible banner for things the user should see once and then be rid of:
 * startup failures, one-time reminders. Lifted out of the dashboard view when
 * that was replaced by the prompt-first home.
 */

import { h } from '../core/dom.js';
import { t } from '../core/i18n.js';
import { icon } from './icon.js';

// warningCard builds a dismissible banner. kind 'warning' is amber (real
// problems); kind 'info' is accent-tinted (reminders). Dismissal is remembered
// in localStorage under the given key; returns null when already dismissed.
export function warningCard(key, text, { kind = 'warning', iconName = 'alert-circle' } = {}) {
  const storageKey = 'of:dismissed:' + key;
  try {
    if (localStorage.getItem(storageKey)) return null;
  } catch { /* localStorage may be unavailable */ }
  const card = h('div', {
    className: 'attention-banner' + (kind === 'info' ? ' attention-banner--info' : ''),
  }, [
    h('span', { innerHTML: icon(iconName) }),
    h('span', { className: 'attention-banner__text' }, text),
    h('button', {
      className: 'btn btn--ghost btn--sm ml-auto',
      'aria-label': t('common.dismiss'),
      onClick: () => {
        try { localStorage.setItem(storageKey, '1'); } catch { /* ignore */ }
        card.remove();
      },
      innerHTML: icon('x'),
    }),
  ]);
  return card;
}
