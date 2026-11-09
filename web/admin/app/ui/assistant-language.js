/**
 * The assistant language select in the Account dialog: the language the
 * assistant writes to you in, which is a separate choice from the interface
 * language. It is stored on the server per user (the server cannot read the
 * browser's saved interface language on its own), so this control loads the
 * current value and saves each change as soon as it is made.
 *
 * The values are the server's vocabulary: '' follows the interface language,
 * 'match' answers in whatever language you write, and anything else is one of the
 * language codes the interface also offers.
 */

import { h } from '../core/dom.js';
import { get, put } from '../core/http.js';
import { SUPPORTED, t } from '../core/i18n.js';
import { runAction } from './actions.js';
import { humanizeError } from './errors.js';
import * as toast from './toast.js';

export const PREFERENCES_URL = '/admin/api/auth/preferences';

/**
 * The select's options in display order: the two relative choices first, then
 * each language under its own name so a person can find theirs.
 * @returns {{ value: string, label: string }[]}
 */
export function assistantLanguageOptions() {
  return [
    { value: '', label: t('account.assistantInterface') },
    { value: 'match', label: t('account.assistantMatch') },
    ...SUPPORTED.map((l) => ({ value: l.code, label: l.label })),
  ];
}

/**
 * The option value to show for a stored preference. A value this build has no
 * option for (the server accepts a language the interface does not list yet)
 * shows as the default instead of leaving the select blank.
 */
export function selectableValue(stored) {
  return assistantLanguageOptions().some((o) => o.value === stored) ? stored : '';
}

/** @returns {HTMLSelectElement} disabled until the stored value has loaded */
export function createAssistantLanguagePicker() {
  let saved = '';
  const select = h('select', {
    className: 'input assistant-language-picker',
    'aria-label': t('account.assistantLanguage'),
    disabled: true,
    onChange: async () => {
      const wanted = select.value;
      select.disabled = true;
      const ok = await runAction(async () => {
        const res = await put(PREFERENCES_URL, { assistant_language: wanted });
        saved = selectableValue(res?.assistant_language ?? wanted);
      }, { success: t('account.assistantSaved') });
      // A failed save must not leave the select claiming a language the server
      // does not have.
      if (ok === false) select.value = saved;
      select.disabled = false;
    },
  }, assistantLanguageOptions().map((o) => h('option', { value: o.value }, o.label)));

  get(PREFERENCES_URL)
    .then((res) => {
      saved = selectableValue(res?.assistant_language ?? '');
      select.value = saved;
      select.disabled = false;
    })
    .catch((err) => {
      // Leave it disabled: showing the default as if it were the stored value
      // would invite a change that overwrites a choice we could not read.
      toast.error(humanizeError(err));
    });

  return select;
}
