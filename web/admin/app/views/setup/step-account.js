/**
 * Setup Step 4: Create admin account.
 */

import { h } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { icon } from '../../ui/icon.js';
import { isSubmitEnter } from '../../ui/a11y.js';
import { formGroup, passwordToggle } from '../../ui/helpers.js';
import { passwordProblem, hasMinLength, hasUpper, hasNumber, hasSpecial } from '../../core/password.js';
import { post } from '../../core/http.js';
import { humanizeError } from '../../ui/errors.js';
import * as toast from '../../ui/toast.js';

export function renderAccount({ body, setHeader, setActions, data: setupData, next: onNext, back: onBack }) {
  const usernameInput = h('input', {
    className: 'input',
    type: 'text',
    placeholder: 'admin',
    value: setupData.username || '',
    autocomplete: 'username',
    autocapitalize: 'off',
    spellcheck: 'false',
  });

  // Only when this admin panel is reachable from the network: the server prints a
  // one-time token to its console so a stranger who finds the open port cannot
  // claim the instance first.
  const tokenInput = setupData.needsSetupToken ? h('input', {
    className: 'input',
    type: 'text',
    placeholder: t('setup.account.tokenPlaceholder'),
    value: setupData.setupToken || '',
    autocomplete: 'off',
    spellcheck: false,
  }) : null;
  const tokenGroup = tokenInput ? formGroup(t('setup.account.tokenLabel'), tokenInput, {
    required: true,
    hint: t('setup.account.tokenHint'),
  }) : null;

  const passwordInput = h('input', {
    className: 'input',
    type: 'password',
    placeholder: t('setup.account.passwordPlaceholder'),
    autocomplete: 'new-password',
  });

  const confirmInput = h('input', {
    className: 'input',
    type: 'password',
    placeholder: t('setup.account.confirm'),
    autocomplete: 'new-password',
  });

  const pwdWrapper = h('div', { className: 'password-wrapper' }, [
    passwordInput,
    passwordToggle(passwordInput),
  ]);

  const confirmWrapper = h('div', { className: 'password-wrapper' }, [
    confirmInput,
    passwordToggle(confirmInput, 'password confirmation'),
  ]);

  // Strength meter bars
  const bars = [0, 1, 2, 3].map(() => h('div', { className: 'password-strength__bar' }));
  const strengthMeter = h('div', { className: 'password-strength' }, bars);
  const strengthText = h('div', { className: 'form-hint' });

  // Interactive requirements checklist
  const REQS = [
    { key: 'length', label: t('setup.account.reqLength'), test: hasMinLength },
    { key: 'upper', label: t('setup.account.reqUpper'), test: hasUpper },
    { key: 'number', label: t('setup.account.reqNumber'), test: hasNumber },
    { key: 'special', label: t('setup.account.reqSpecial'), test: hasSpecial },
  ];

  const reqEls = REQS.map((req) => {
    const iconEl = h('span', { className: 'password-req__icon' });
    const el = h('div', { className: 'password-req', 'data-req': req.key }, [
      iconEl,
      h('span', {}, req.label),
    ]);
    return { el, iconEl, test: req.test };
  });

  const reqList = h('div', { className: 'password-reqs' }, reqEls.map(r => r.el));

  function checkStrength(pwd) {
    let score = 0;

    reqEls.forEach((req) => {
      const met = req.test(pwd);
      if (met) score++;
      req.el.className = `password-req${met ? ' met' : ''}`;
      req.iconEl.innerHTML = met ? icon('check') : '';
    });

    bars.forEach((bar, i) => {
      bar.className = 'password-strength__bar';
      if (i < score) {
        if (score <= 1) bar.classList.add('filled');
        else if (score <= 2) bar.classList.add('medium');
        else bar.classList.add('strong');
      }
    });

    const labels = ['', t('setup.account.weak'), t('setup.account.fair'), t('setup.account.good'), t('setup.account.strong')];
    strengthText.textContent = pwd ? labels[score] || '' : '';

    return score;
  }

  passwordInput.addEventListener('input', () => {
    checkStrength(passwordInput.value);
  });

  // Same treatment as the rest of the admin's forms: a toast says what is
  // wrong, and the field that is wrong turns red until it is edited.
  function reject(input, message) {
    toast.warning(message);
    input.classList.add('input--error');
    input.focus();
    return false;
  }

  function validate() {
    if (tokenInput && !tokenInput.value.trim()) return reject(tokenInput, t('setup.account.tokenRequired'));
    const username = usernameInput.value.trim();
    const password = passwordInput.value;
    const confirm = confirmInput.value;

    if (!username) return reject(usernameInput, t('setup.account.usernameRequired'));
    const problem = passwordProblem(password);
    if (problem) return reject(passwordInput, problem);
    if (password !== confirm) return reject(confirmInput, t('setup.account.mismatch'));
    return true;
  }

  // The token is checked as soon as it is entered, so a typo is caught here, next
  // to the field, rather than a step later as a generic "failed to load" error.
  let checking = false;
  async function submit() {
    if (checking || !validate()) return;
    if (tokenInput) {
      checking = true;
      try {
        await post('/admin/api/setup/token', {}, { headers: { 'X-Setup-Token': tokenInput.value.trim() } });
      } catch (err) {
        checking = false;
        if (err && err.status === 410) { window.location.reload(); return; }
        reject(tokenInput, err && err.status === 403
          ? t('setup.account.tokenWrong')
          : humanizeError(err));
        return;
      }
      checking = false;
      setupData.setupToken = tokenInput.value.trim();
    }
    setupData.username = usernameInput.value.trim();
    setupData.password = passwordInput.value;
    onNext();
  }

  // Enter key on any input submits; editing a field clears its error state.
  function onKey(e) {
    if (isSubmitEnter(e)) {
      e.preventDefault();
      submit();
    }
  }
  for (const input of [tokenInput, usernameInput, passwordInput, confirmInput].filter(Boolean)) {
    input.addEventListener('keydown', onKey);
    input.addEventListener('input', () => input.classList.remove('input--error'));
  }

  const greeting = setupData.displayName
    ? t('setup.account.greeting', { name: setupData.displayName })
    : t('setup.account.greetingDefault');

  setHeader(greeting, t('setup.account.lede'));

  const pwdGroup = formGroup(t('login.password'), pwdWrapper, { required: true });
  pwdGroup.append(strengthMeter, strengthText, reqList);

  body.append(
    ...(tokenGroup ? [tokenGroup] : []),
    formGroup(t('login.username'), usernameInput, { required: true }),
    pwdGroup,
    formGroup(t('setup.account.confirm'), confirmWrapper, { required: true }),
  );

  setActions([
    h('button', { className: 'btn btn--ghost', onClick: onBack }, t('common.back')),
    h('button', { className: 'btn btn--primary', onClick: submit }, t('common.continue')),
  ]);

  (tokenInput || usernameInput).focus();
}
