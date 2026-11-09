/**
 * The interface language select, shared by the sign-in bar and the Account
 * dialog (setup has its own step, views/setup/step-language.js). Picking a language saves it and reloads, so every string is
 * rebuilt from the new catalog.
 */

import { h } from '../core/dom.js';
import { SUPPORTED, getLocale, setLocale, t } from '../core/i18n.js';

/**
 * @param {{ compact?: boolean }} [opts] compact is the short control for the top
 *   bar; the default matches the other selects in forms and dialogs.
 * @returns {HTMLSelectElement}
 */
export function createLanguagePicker({ compact = false } = {}) {
  const select = h('select', {
    className: compact ? 'input input--sm language-picker' : 'input language-picker',
    'aria-label': t('language.label'),
    title: t('language.label'),
    onChange: () => {
      if (setLocale(select.value)) window.location.reload();
    },
  }, SUPPORTED.map((l) => h('option', { value: l.code }, l.label)));
  select.value = getLocale();
  return select;
}
