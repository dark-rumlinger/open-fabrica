/**
 * Modal dialog component.
 */

import { h, clear } from '../core/dom.js';
import { t } from '../core/i18n.js';
import { icon } from './icon.js';
import { isSubmitEnter } from './a11y.js';
import * as toast from './toast.js';
import { humanizeError } from './errors.js';

let currentOverlay = null;
let currentKeyHandler = null;
let previouslyFocused = null;
// Fires once when the current modal closes, by any path (X, ESC, or an action).
// Lets a caller tear down side effects tied to the modal being open, e.g. the
// About dialog stopping its background music.
let currentOnClose = null;

// Selector for elements that participate in the Tab focus order.
const FOCUSABLE = [
  'a[href]',
  'button:not([disabled])',
  'input:not([disabled]):not([type="hidden"])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(',');

/**
 * Show a modal dialog.
 * @param {string} title
 * @param {HTMLElement|string} content - Body content
 * @param {Array<{label: string, className?: string, onClick: Function}>} [actions]
 * @param {Object} [opts] - Options: { wide, onClose, initialFocus: 'cancel', enterSubmits: false, ariaLabel }
 * @returns {Function} close function
 */
export function show(title, content, actions = [], opts = {}) {
  close(); // Close any existing modal (fires its onClose, then we set ours below)
  currentOnClose = typeof opts.onClose === 'function' ? opts.onClose : null;

  const closeBtn = h('button', {
    className: 'btn btn--icon',
    onClick: close,
    innerHTML: icon('x'),
    'aria-label': t('modal.closeDialog'),
  });

  const titleId = 'modal-title-' + Math.random().toString(36).slice(2, 9);
  const header = h('div', { className: 'modal__header' }, [
    h('h3', { className: 'modal__title', id: titleId }, title),
    closeBtn,
  ]);

  const body = h('div', { className: 'modal__body' });
  if (typeof content === 'string') {
    body.textContent = content;
  } else if (content instanceof HTMLElement) {
    body.appendChild(content);
  }

  const footer = h('div', { className: 'modal__footer' },
    actions.map(a => {
      // Guard against double-submits: while an action's handler is in flight
      // (e.g. an async POST that creates a project), ignore further clicks and
      // disable the button so a fast double-click can't fire it twice.
      let pending = false;
      const btn = h('button', {
        className: a.className || 'btn',
        onClick: async () => {
          if (pending) return;
          pending = true;
          btn.disabled = true;
          // The overlay this click belongs to. If the handler opens a follow-up
          // modal, close() below would otherwise shut that new one instead.
          const owner = currentOverlay;
          try {
            const result = await Promise.resolve(a.onClick());
            if (result !== false && currentOverlay === owner) close();
          } catch (err) {
            // A handler that throws would otherwise be an unhandled rejection
            // with no feedback; keep the modal open so the input survives.
            toast.error(humanizeError(err));
          } finally {
            pending = false;
            btn.disabled = false;
          }
        },
      }, a.label);
      return btn;
    })
  );

  const modalClass = 'modal' + (opts.wide ? ' modal--wide' : '');
  const modal = h('div', {
    className: modalClass,
    role: 'dialog',
    'aria-modal': 'true',
    // A dialog with no visible title (the About panel) still needs a name.
    ...(title ? { 'aria-labelledby': titleId } : { 'aria-label': opts.ariaLabel || 'Dialog' }),
  }, [header, body, footer]);

  // Modals close only via the X button, an explicit action, or ESC.
  // Outside-click dismissal is deliberately disabled, accidental clicks on
  // the scrim should not discard unsaved form input.
  const overlay = h('div', { className: 'modal-overlay' }, [modal]);

  previouslyFocused = document.activeElement;
  document.body.appendChild(overlay);
  document.body.style.overflow = 'hidden';
  currentOverlay = overlay;

  // Animate in and auto-focus first input
  requestAnimationFrame(() => {
    overlay.classList.add('visible');
    // Destructive confirms start on Cancel so a stray Enter cannot delete.
    const firstInput = modal.querySelector('input:not([type="checkbox"]):not([type="hidden"]), textarea, select');
    const cancel = footer.querySelector('button');
    if (opts.initialFocus === 'cancel' && cancel) cancel.focus();
    else if (firstInput) firstInput.focus();
    else closeBtn.focus();
  });

  // Enter in a single-line field runs the dialog's primary action, like a form
  // submit. Textareas, buttons and checkboxes keep their own Enter behaviour.
  if (opts.enterSubmits !== false) {
    modal.addEventListener('keydown', (e) => {
      if (!isSubmitEnter(e) || e.defaultPrevented) return;
      const t = e.target;
      if (!t || t.tagName !== 'INPUT') return;
      if (['button', 'submit', 'checkbox', 'radio', 'file'].includes(t.type)) return;
      const primary = footer.querySelector('.btn--primary');
      if (primary && !primary.disabled) {
        e.preventDefault();
        primary.click();
      }
    });
  }

  // ESC to close + Tab trap.
  currentKeyHandler = (e) => {
    if (e.key === 'Escape') {
      if (e.isComposing) return; // Escape cancels IME composition, not the dialog
      close();
      return;
    }
    if (e.key !== 'Tab') return;
    const focusable = Array.from(modal.querySelectorAll(FOCUSABLE))
      .filter(el => el.offsetParent !== null);
    if (focusable.length === 0) {
      e.preventDefault();
      return;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  };
  document.addEventListener('keydown', currentKeyHandler);

  return close;
}

/**
 * Close the current modal.
 */
export function close() {
  // Snapshot + clear the hook first so a re-entrant close() (or an onClose that
  // opens another modal) can't fire it twice.
  const onClose = currentOnClose;
  currentOnClose = null;
  if (currentKeyHandler) {
    document.removeEventListener('keydown', currentKeyHandler);
    currentKeyHandler = null;
  }
  document.body.style.overflow = '';
  if (currentOverlay) {
    currentOverlay.classList.remove('visible');
    const overlay = currentOverlay;
    setTimeout(() => {
      if (overlay.parentNode) {
        overlay.parentNode.removeChild(overlay);
      }
    }, 200);
    currentOverlay = null;
  }
  if (previouslyFocused && typeof previouslyFocused.focus === 'function') {
    previouslyFocused.focus();
  }
  previouslyFocused = null;
  if (onClose) {
    try { onClose(); } catch (_) { /* teardown must never block close */ }
  }
}

/** Returns a standard cancel/close button for modal actions. */
export function cancelBtn(label = t('common.cancel')) {
  return { label, className: 'btn', onClick: () => {} };
}

/**
 * Show a confirmation dialog for an action that is consequential but not
 * destructive (a primary button, not a red one).
 */
export function confirm(title, message, onConfirm, confirmLabel = t('common.confirm')) {
  const body = h('p', { className: 'text-secondary' }, message);
  return show(title, body, [
    cancelBtn(),
    { label: confirmLabel, className: 'btn btn--primary', onClick: onConfirm },
  ], { initialFocus: 'cancel' });
}

// "Type X to confirm." with X highlighted. The catalog sentence carries
// {expected} so each language can place it where its word order needs it. When
// onPick is given, X becomes a button that fills the field on click, so a quick
// delete needs no typing; without it X is plain bold text.
function typeToConfirmLine(expected, onPick) {
  const marker = '\u0000';
  const [before, after = ''] = t('modal.typeToConfirm', { expected: marker }).split(marker);
  const name = onPick
    ? h('button', {
        type: 'button',
        className: 'confirm-name',
        onClick: onPick,
        title: t('modal.clickToFill'),
        'aria-label': t('modal.clickToFill'),
      }, expected)
    : h('strong', {}, expected);
  return [before, name, after];
}

/**
 * Confirmation for an irreversible, high-blast-radius action (deleting a
 * project): the confirm button stays disabled until the person types `expected`
 * exactly, so a reflex click cannot do it.
 */
export function confirmByTyping(title, message, expected, onConfirm, confirmLabel = t('common.delete')) {
  const input = h('input', {
    className: 'input',
    type: 'text',
    autocomplete: 'off',
    autocapitalize: 'off',
    spellcheck: 'false',
    'aria-label': t('modal.typeToConfirmAria', { expected }),
    placeholder: expected,
  });
  // Clicking the highlighted name fills the field for people who would rather
  // not retype it. The 'input' event drives the same enable check as typing, so
  // the confirm button still gates on an exact match.
  const fill = () => {
    input.value = expected;
    input.dispatchEvent(new Event('input', { bubbles: true }));
    input.focus();
  };
  const body = h('div', {}, [
    h('p', { className: 'text-secondary mb-3' }, message),
    h('p', { className: 'text-sm mb-2' }, typeToConfirmLine(expected, fill)),
    input,
  ]);
  show(title, body, [
    cancelBtn(),
    { label: confirmLabel, className: 'btn btn--danger', onClick: onConfirm },
  ], { enterSubmits: false });
  // The confirm button is the last one in the footer; keep it off until the
  // name matches.
  requestAnimationFrame(() => {
    const buttons = document.querySelectorAll('.modal__footer button');
    const confirmBtn = buttons[buttons.length - 1];
    if (!confirmBtn) return;
    confirmBtn.disabled = true;
    input.addEventListener('input', () => { confirmBtn.disabled = input.value.trim() !== expected; });
    input.addEventListener('keydown', (e) => {
      if (isSubmitEnter(e) && input.value.trim() === expected) confirmBtn.click();
    });
  });
}

/**
 * Show a danger confirmation dialog.
 */
export function confirmDanger(title, message, onConfirm, confirmLabel = t('common.delete')) {
  const body = h('p', { className: 'text-secondary' }, message);
  return show(title, body, [
    cancelBtn(),
    { label: confirmLabel, className: 'btn btn--danger', onClick: onConfirm },
  ], { initialFocus: 'cancel' });
}
