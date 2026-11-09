/**
 * Default project example prompts, offered as starter chips on the new-project
 * composer. Single source of truth for the wording.
 */

import { h } from '../core/dom.js';
import { t } from '../core/i18n.js';

// The wording lives in the locale catalogs (examples.N.label / examples.N.text), so
// the prompt a chip drops into the box is in the interface language.
const EXAMPLE_COUNT = 8;
const PROJECT_EXAMPLES = Array.from({ length: EXAMPLE_COUNT }, (_, i) => ({
  label: t(`examples.${i + 1}.label`),
  text: t(`examples.${i + 1}.text`),
}));

/**
 * Render the example chips. Clicking a chip fills the given prompt textarea,
 * triggers its input handler (for char counts), and refocuses it.
 * @param {HTMLTextAreaElement} descInput target prompt textarea
 * @returns {HTMLElement} the chips container
 */
export function promptChips(descInput) {
  return h('div', { className: 'prompt-chips' },
    PROJECT_EXAMPLES.map(e => h('button', {
      type: 'button',
      className: 'prompt-chip',
      onClick: () => {
        descInput.value = e.text;
        descInput.dispatchEvent(new Event('input'));
        descInput.focus();
      },
    }, e.label))
  );
}
