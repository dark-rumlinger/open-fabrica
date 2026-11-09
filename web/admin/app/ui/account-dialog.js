/**
 * The Account dialog: change your own password, or sign out every other browser
 * and device. Both calls answer with a fresh token at the new session version, so
 * the browser you are in stays signed in while all others are cut off.
 */

import { h } from '../core/dom.js';
import { put, post, TOKEN_KEY } from '../core/http.js';
import * as toast from './toast.js';
import * as modal from './modal.js';
import { formGroup } from './helpers.js';
import { runAction } from './actions.js';
import { createFormModal } from './form-modal.js';
import { createLanguagePicker } from './language-picker.js';
import { createAssistantLanguagePicker } from './assistant-language.js';
import { t } from '../core/i18n.js';

import { passwordProblem, PASSWORD_HINT } from '../core/password.js';

// Re-exported so existing importers (and tests) keep working.
export { passwordProblem };

// Keep this browser signed in on the replacement token the server just minted.
function adoptSession(res) {
  if (res && res.token) localStorage.setItem(TOKEN_KEY, res.token);
}

export function showAccountModal() {
  const body = h('div', {}, [
    h('p', { className: 'form-hint' }, t('account.intro')),
    h('div', { className: 'flex gap-2 mt-3' }, [
      h('button', { className: 'btn btn--primary', onClick: showChangePassword }, t('account.changePassword')),
      h('button', { className: 'btn btn--secondary', onClick: confirmRevoke }, t('account.signOutOthers')),
    ]),
    h('div', { className: 'mt-3' }, [formGroup(t('account.interfaceLanguage'), createLanguagePicker())]),
    h('div', {}, [formGroup(t('account.assistantLanguage'), createAssistantLanguagePicker(), {
      hint: t('account.assistantLanguageHint'),
    })]),
  ]);
  modal.show(t('account.title'), body, [modal.cancelBtn(t('common.close'))]);
}

function showChangePassword() {
  const current = h('input', { className: 'input', type: 'password', autocomplete: 'current-password' });
  const next = h('input', { className: 'input', type: 'password', autocomplete: 'new-password' });
  const confirm = h('input', { className: 'input', type: 'password', autocomplete: 'new-password' });

  createFormModal({
    title: t('account.changePassword'),
    submitLabel: t('account.changePassword'),
    fields: [
      formGroup(t('account.currentPassword'), current, { required: true }),
      formGroup(t('account.newPassword'), next, {
        required: true,
        hint: PASSWORD_HINT,
      }),
      formGroup(t('account.repeatPassword'), confirm, { required: true }),
    ],
    validate: () => {
      if (!current.value) return t('account.enterCurrent');
      const problem = passwordProblem(next.value);
      if (problem) return problem;
      if (next.value !== confirm.value) return t('account.mismatch');
      return null;
    },
    submit: async () => {
      adoptSession(await put('/admin/api/auth/password', {
        current_password: current.value,
        new_password: next.value,
      }));
    },
    success: t('account.passwordChanged'),
  });
}

function confirmRevoke() {
  modal.show(t('account.signOutOthers'),
    h('p', {}, t('account.revokeBody')),
    [
      modal.cancelBtn(t('common.cancel')),
      {
        label: t('account.signOutOthers'),
        className: 'btn btn--danger',
        onClick: () => runAction(async () => {
          adoptSession(await post('/admin/api/auth/revoke-sessions', {}));
        }, { success: t('account.othersSignedOut') }),
      },
    ]);
}
