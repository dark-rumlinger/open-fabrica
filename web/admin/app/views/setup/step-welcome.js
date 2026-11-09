/**
 * Setup Step 3: Welcome, and what to call you.
 */

import { h } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { formGroup } from '../../ui/helpers.js';
import { isSubmitEnter } from '../../ui/a11y.js';
import * as toast from '../../ui/toast.js';

export function renderWelcome({ body, setHeader, setActions, data, next, back }) {
  const nameInput = h('input', {
    className: 'input',
    type: 'text',
    placeholder: t('setup.welcome.placeholder'),
    value: data.displayName || '',
    onkeydown: (e) => {
      if (isSubmitEnter(e)) {
        e.preventDefault();
        submit();
      }
    },
    oninput: () => nameInput.classList.remove('input--error'),
  });

  function submit() {
    const name = nameInput.value.trim();
    if (!name) {
      toast.warning(t('setup.welcome.enterName'));
      nameInput.classList.add('input--error');
      nameInput.focus();
      return;
    }
    data.displayName = name;
    next();
  }

  setHeader(t('setup.welcome.title'), t('setup.welcome.lede'));
  body.appendChild(formGroup(t('setup.welcome.nameLabel'), nameInput, { required: true }));
  setActions([
    h('button', { className: 'btn btn--ghost', onClick: back }, t('common.back')),
    h('button', { className: 'btn btn--primary', onClick: submit }, t('common.continue')),
  ]);

  nameInput.focus();
}
