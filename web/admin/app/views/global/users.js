import { h } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { get, post, put, del } from '../../core/http.js';
import * as modal from '../../ui/modal.js';
import * as state from '../../core/state.js';
import { defineView } from '../../core/view.js';
import { card, pageHeader, pageBody } from '../../ui/card.js';
import { createListView } from '../../ui/list-view.js';
import { formGroup } from '../../ui/helpers.js';
import { runAction } from '../../ui/actions.js';
import { createFormModal } from '../../ui/form-modal.js';
import { passwordProblem, PASSWORD_HINT } from '../../core/password.js';

export const renderUsers = defineView((ctx, container) => {
  container.replaceChildren();

  const body = pageBody();
  const list = createListView({
    scope: ctx.scope,
    container: body,
    fetch: () => get('/admin/api/users'),
    renderItem: (u) => renderUser(u, () => list.reload()),
    filter: true,
    empty: {
      iconName: 'user',
      title: t('users.empty.title'),
      description: t('users.empty.description'),
      action: { label: t('users.add'), onClick: () => showAddModal(() => list.reload()) },
    },
    errorMsg: t('users.loadFailed'),
  });

  const header = pageHeader({
    title: t('nav.users'),
    actions: [{
      label: t('users.add'),
      kind: 'primary',
      iconName: 'plus',
      onClick: () => showAddModal(list.reload),
    }],
  });

  container.appendChild(header);
  container.appendChild(body);
  list.reload();
});

function renderUser(u, reload) {
  const isSelf = u.id === state.currentUserId();
  const name = u.display_name || u.username;
  return card({
    title: isSelf ? t('users.you', { name }) : name,
    subtitle: u.display_name ? u.username : null,
    iconName: 'user',
    actions: [
      { label: t('common.edit'), iconName: 'edit', onClick: () => showEditModal(u, reload) },
      { label: t('common.delete'), iconName: 'trash', kind: 'danger', hidden: isSelf, onClick: () => confirmDelete(u, reload) },
    ],
  });
}

function showAddModal(reload) {
  const usernameInput = h('input', { className: 'input', placeholder: t('login.username'), autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false' });
  const displayNameInput = h('input', { className: 'input', placeholder: t('users.displayName'), autocomplete: 'off' });
  const passwordInput = h('input', { className: 'input', type: 'password', placeholder: t('login.password'), autocomplete: 'new-password' });

  createFormModal({
    title: t('users.add'),
    fields: [
      formGroup(t('login.username'), usernameInput, {
        required: true,
        hint: t('users.usernameHint'),
      }),
      formGroup(t('users.displayName'), displayNameInput, {
        hint: t('users.displayNameHint'),
      }),
      formGroup(t('login.password'), passwordInput, {
        required: true,
        hint: PASSWORD_HINT,
      }),
    ],
    submitLabel: t('users.create'),
    validate: () => {
      usernameInput.classList.remove('input--error');
      passwordInput.classList.remove('input--error');
      if (!usernameInput.value.trim()) {
        usernameInput.classList.add('input--error');
        return t('setup.account.usernameRequired');
      }
      const pwErr = passwordInput.value ? passwordProblem(passwordInput.value) : t('users.passwordRequired');
      if (pwErr) {
        passwordInput.classList.add('input--error');
        return pwErr;
      }
      return null;
    },
    submit: () => post('/admin/api/users', {
      username: usernameInput.value.trim(),
      display_name: displayNameInput.value.trim(),
      password: passwordInput.value,
    }),
    success: t('users.created'),
    onDone: reload,
  });
}

function showEditModal(user, reload) {
  const displayNameInput = h('input', { className: 'input', value: user.display_name || '', placeholder: t('users.displayName'), autocomplete: 'off' });
  const passwordInput = h('input', { className: 'input', type: 'password', placeholder: t('users.newPasswordPlaceholder'), autocomplete: 'new-password' });

  createFormModal({
    title: t('users.editTitle', { username: user.username }),
    fields: [
      formGroup(t('users.displayName'), displayNameInput, { hint: t('users.displayNameEditHint') }),
      formGroup(t('login.password'), passwordInput, { hint: t('users.passwordEditHint') }),
    ],
    validate: () => {
      passwordInput.classList.remove('input--error');
      const pwErr = passwordInput.value ? passwordProblem(passwordInput.value) : null;
      if (pwErr) {
        passwordInput.classList.add('input--error');
        return pwErr;
      }
      return null;
    },
    submit: () => {
      const body = {};
      const dn = displayNameInput.value.trim();
      if (dn) body.display_name = dn;
      if (passwordInput.value) body.password = passwordInput.value;
      return put(`/admin/api/users/${user.id}`, body);
    },
    success: t('users.updated'),
    onDone: reload,
  });
}

function confirmDelete(user, reload) {
  modal.confirmDanger(
    t('users.deleteTitle'),
    t('users.deleteBody', { username: user.username }),
    () => runAction(() => del(`/admin/api/users/${user.id}`), { success: t('users.deleted'), onDone: reload }),
  );
}
