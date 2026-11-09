/**
 * Collapses the modal.show + cancel/submit pair every create/edit dialog
 * hand-rolled: stack `fields`, validate, then run `submit` through runAction;
 * the dialog stays open on validation or request failure. `validate` returns
 * an error string, shown inline under the fields (and announced), or `false`
 * when the caller already surfaced the problem itself; `submit` may resolve
 * `false` to stay open without a toast. Enter in a single-line field submits
 * (modal.js). Callers keep building their own inputs; this owns only the
 * modal plumbing.
 */

import { h } from '../core/dom.js';
import { t } from '../core/i18n.js';
import * as modal from './modal.js';
import { runAction } from './actions.js';

export function createFormModal({ title, fields, submitLabel = t('common.save'), validate, submit, success, onDone }) {
  const errorLine = h('p', { className: 'form-error', role: 'alert', hidden: true });
  const body = h('div', {}, [...fields, errorLine]);
  // Typing again clears the stale message.
  body.addEventListener('input', () => { errorLine.hidden = true; });

  modal.show(title, body, [
    modal.cancelBtn(),
    {
      label: submitLabel,
      className: 'btn btn--primary',
      onClick: () => {
        errorLine.hidden = true;
        if (validate) {
          const err = validate();
          if (typeof err === 'string' && err) {
            errorLine.textContent = err;
            errorLine.hidden = false;
            return false;
          }
          if (err === false) return false;
        }
        return runAction(submit, { success, onDone });
      },
    },
  ]);
}
