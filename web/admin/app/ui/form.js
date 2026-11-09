/**
 * Form submission helpers: the disable-button -> busy-label -> try/catch ->
 * humanized-toast -> re-enable loop in one place. Modal action onClick
 * handlers use runAction from ui/actions.js instead.
 */

import * as toast from './toast.js';
import { humanizeError } from './errors.js';

function messageOf(err) {
  if (!err) return '';
  return err.message !== undefined ? err.message : err;
}

/**
 * Own a <form> submit lifecycle: double-submit guard, busy label, humanized
 * error toast, button restored in every outcome. Options: busyLabel,
 * keepDisabled (success navigates away, don't flicker back), errorPrefix,
 * onError (called with the error after the toast, e.g. to refocus a field),
 * onSubmit (throw -> toast; return false -> handled, no toast). Returns
 * { submit, restore }; `form` may be null to wire a bare button.
 */
export function wireSubmit(form, btn, opts = {}) {
  const { onSubmit, busyLabel, keepDisabled = false, errorPrefix } = opts;
  const idleLabel = btn ? btn.textContent : '';
  let busy = false;

  function restore() {
    busy = false;
    if (btn) {
      btn.disabled = false;
      if (busyLabel) btn.textContent = idleLabel;
    }
  }

  async function submit(e) {
    if (e && typeof e.preventDefault === 'function') e.preventDefault();
    if (busy) return;
    busy = true;
    if (btn) {
      btn.disabled = true;
      if (busyLabel) btn.textContent = busyLabel;
    }
    try {
      const ok = await onSubmit();
      if (ok === false) { restore(); return; }
      if (!keepDisabled) restore();
    } catch (err) {
      const msg = humanizeError(messageOf(err));
      toast.error(errorPrefix ? `${errorPrefix}: ${msg}` : msg);
      restore();
      if (typeof opts.onError === 'function') opts.onError(err, msg);
    }
  }

  if (form) form.addEventListener('submit', submit);
  return { submit, restore };
}
