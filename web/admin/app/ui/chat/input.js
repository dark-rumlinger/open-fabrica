/**
 * Chat input bar with auto-expanding textarea. The send button doubles as a
 * stop button while work is in flight. "Working" is a SET of reasons, not a
 * flag: a chat turn and the build pipeline can be in flight at once, and a
 * bool would let whichever finished first clear the other's button.
 */

import { h } from '../../core/dom.js';
import { t } from '../../core/i18n.js';
import { icon } from '../icon.js';
import { isSubmitEnter } from '../a11y.js';

/**
 * Create a chat input component.
 * @param {Function} onSend - Callback with message text.
 * @param {Function} [onStop] - Callback when the owner clicks stop. Without it
 *   the button never enters stop mode.
 * @param {{ draftKey?: string }} [opts] - draftKey persists the unsent text in
 *   localStorage so a reload or tab switch does not lose it.
 * @returns {{ element: HTMLElement, setWorking: Function, isWorking: Function,
 *   setStopping: Function, isStopping: Function, disable: Function,
 *   enable: Function, focus: Function, clear: Function }}
 */
export function createChatInput(onSend, onStop, opts = {}) {
  const textarea = h('textarea', {
    className: 'chat-input__textarea',
    placeholder: t('chat.inputPlaceholder'),
    rows: '3',
    'aria-label': t('chat.inputLabel'),
  });

  const sendBtn = h('button', {
    className: 'chat-input__send',
    innerHTML: icon('send'),
    title: t('chat.send'),
    'aria-label': t('chat.send'),
    disabled: true,
  });

  const wrapper = h('div', { className: 'chat-input__wrapper' }, [
    textarea,
    sendBtn,
  ]);

  const element = h('div', { className: 'chat-input' }, [wrapper]);

  // Reasons something is in flight; the button stops while any is present.
  const working = new Set();
  const canStop = typeof onStop === 'function';
  const isWorking = () => canStop && working.size > 0;
  // A stop is in flight. The engine winds up the turn already running rather
  // than dropping it, so the button says so instead of looking unresponsive.
  let stopping = false;

  // Unsent text survives a reload or a tab switch. Storage can be blocked, so
  // every access is guarded and the input works the same without it.
  const draftKey = opts.draftKey;
  function readDraft() {
    try { return draftKey ? (localStorage.getItem(draftKey) || '') : ''; } catch { return ''; }
  }
  function writeDraft(text) {
    if (!draftKey) return;
    try {
      if (text) localStorage.setItem(draftKey, text);
      else localStorage.removeItem(draftKey);
    } catch { /* storage blocked */ }
  }

  // Auto-expand textarea
  function autoResize() {
    textarea.style.height = 'auto';
    const MIN_INPUT_HEIGHT = 72;
    const MAX_INPUT_HEIGHT = 320;
    const next = Math.max(MIN_INPUT_HEIGHT, Math.min(textarea.scrollHeight, MAX_INPUT_HEIGHT));
    textarea.style.height = next + 'px';
    syncButton();
  }

  // Render the button for the current mode. In stop mode it stays enabled
  // whatever the textarea holds: the owner is stopping work, not sending text.
  function syncButton() {
    if (isWorking()) {
      sendBtn.classList.add('chat-input__send--stop');
      sendBtn.innerHTML = icon('stop');
      sendBtn.title = stopping ? t('chat.stopping') : t('chat.stop');
      sendBtn.setAttribute('aria-label', sendBtn.title);
      sendBtn.disabled = stopping;
      return;
    }
    sendBtn.classList.remove('chat-input__send--stop');
    sendBtn.innerHTML = icon('send');
    sendBtn.title = t('chat.send');
    sendBtn.setAttribute('aria-label', t('chat.send'));
    sendBtn.disabled = !textarea.value.trim();
  }

  textarea.addEventListener('input', () => {
    writeDraft(textarea.value);
    autoResize();
  });

  // Handle send
  function doSend() {
    const text = textarea.value.trim();
    if (!text) return;
    onSend(text);
    textarea.value = '';
    writeDraft('');
    autoResize();
  }

  const savedDraft = readDraft();
  if (savedDraft) {
    textarea.value = savedDraft;
    autoResize();
  }

  // Enter to send, Shift+Enter for newline. Enter never stops: a stop is
  // deliberate, and the owner is typically mid-sentence when work is running.
  textarea.addEventListener('keydown', (e) => {
    // Enter while an IME is composing confirms the candidate, not the message.
    if (isSubmitEnter(e)) {
      e.preventDefault();
      if (isWorking()) return;
      doSend();
    }
  });

  sendBtn.addEventListener('click', () => {
    if (isWorking()) {
      onStop();
      return;
    }
    doSend();
  });

  return {
    element,
    /**
     * Mark a reason as in flight (or finished). The button shows stop while any
     * reason is outstanding.
     * @param {string} reason - Independent source of work, e.g. 'chat' | 'engine'.
     * @param {boolean} active
     */
    setWorking(reason, active) {
      if (active) working.add(reason);
      else working.delete(reason);
      syncButton();
    },
    isWorking,
    /**
     * Mark a stop request as in flight, so the button reads "Stopping…" and
     * can't be clicked again while the engine winds up.
     * @param {boolean} active
     */
    setStopping(active) {
      stopping = !!active;
      syncButton();
    },
    isStopping: () => stopping,
    disable() {
      textarea.disabled = true;
      sendBtn.disabled = true;
    },
    enable() {
      textarea.disabled = false;
      syncButton();
    },
    focus() {
      textarea.focus();
    },
    clear() {
      textarea.value = '';
      writeDraft('');
      autoResize();
    },
    /**
     * Put text back after a send failed, unless the owner has already started
     * typing something new.
     */
    restore(text) {
      if (textarea.value.trim()) return;
      textarea.value = text;
      writeDraft(text);
      autoResize();
    },
  };
}
