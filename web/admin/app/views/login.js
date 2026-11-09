/**
 * Login page view.
 */

import { h, render } from '../core/dom.js';
import { post, SESSION_EXPIRED_KEY } from '../core/http.js';
import { t } from '../core/i18n.js';

import { wireSubmit } from '../ui/form.js';
import { formGroup, passwordToggle } from '../ui/helpers.js';
import { runSplash } from '../ui/splash.js';
import { createAuthShell, authCard } from '../ui/auth-shell.js';

export function renderLogin(container) {
  const usernameInput = h('input', {
    className: 'input',
    type: 'text',
    placeholder: t('login.username'),
    autocomplete: 'username',
    autocapitalize: 'off',
    spellcheck: 'false',
    required: true,
  });

  const passwordInput = h('input', {
    className: 'input',
    type: 'password',
    placeholder: t('login.password'),
    autocomplete: 'current-password',
    required: true,
  });

  // Shown under the fields so a wrong password or a lockout is read where the
  // eyes already are, not only in a toast that fades.
  const errorLine = h('p', { className: 'form-error', role: 'alert', hidden: true });

  // http.js leaves this flag when it ends a session on a 401, so the person
  // is told why they are looking at the login screen again.
  let expired = false;
  try {
    expired = sessionStorage.getItem(SESSION_EXPIRED_KEY) === '1';
    sessionStorage.removeItem(SESSION_EXPIRED_KEY);
  } catch { /* storage blocked */ }

  const submitBtn = h('button', {
    className: 'btn btn--primary',
    type: 'submit',
  }, t('login.signIn'));

  // Fields cascade in once the card is revealed (auth.css .auth-card__field).
  const fields = [
    h('p', { className: 'auth-card__lede' }, t('login.lede')),
    ...(expired ? [h('p', { className: 'form-hint form-hint--info', role: 'status' }, t('login.sessionEnded'))] : []),
    formGroup(t('login.username'), usernameInput),
    formGroup(t('login.password'), h('div', { className: 'password-wrapper' }, [passwordInput, passwordToggle(passwordInput)])),
    errorLine,
  ];
  fields.forEach((el, i) => {
    el.classList.add('auth-card__field');
    el.style.setProperty('--i', String(i));
  });

  // The whole card is the <form>, so Enter submits from either field even
  // though the button sits in the footer.
  const { card } = authCard({
    tag: 'form',
    compact: true,
    className: 'auth-card--preboot',
    title: t('login.title'),
    body: fields,
    footer: [submitBtn],
  });

  // Success navigates + full-reloads the app shell, so keep the button
  // disabled rather than flickering it back to "Sign in".
  wireSubmit(card, submitBtn, {
    busyLabel: t('login.signingIn'),
    keepDisabled: true,
    onError: (_err, msg) => {
      errorLine.textContent = msg;
      errorLine.hidden = false;
      passwordInput.value = '';
      passwordInput.focus();
    },
    onSubmit: async () => {
      errorLine.hidden = true;
      const data = await post('/admin/api/auth/login', {
        username: usernameInput.value.trim(),
        password: passwordInput.value,
      });
      localStorage.setItem('of_token', data.token);
      localStorage.setItem('of_user', JSON.stringify(data.user));
      // Full load rather than a route change: the shell has to be rebuilt for
      // the session we just opened.
      window.location.href = '/new';
    },
  });

  // Mounted hidden behind the splash; runSplash() lifts the preboot state
  // once the logo has flown into the bar.
  const shell = createAuthShell({ children: [card] });
  render(container, shell.root);

  // Brand splash that morphs the logo into the bar's mark, mirroring setup.
  // runSplash focuses the first input once it resolves.
  runSplash(shell.root, card, {
    target: shell.brandMark,
    holdMs: 1300, // returning users see login often, so keep it brisk (~50%)
  });
}
