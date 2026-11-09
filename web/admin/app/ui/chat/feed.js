/**
 * Chat feed container with auto-scroll behavior.
 */

import { h, clear } from '../../core/dom.js';
import { t, tn } from '../../core/i18n.js';
import { createAvatar } from './avatar.js';
import { icon } from '../icon.js';

/**
 * Create a chat feed container.
 * @returns {{ element: HTMLElement, append: Function, clear: Function, scrollToBottom: Function }}
 */
export function createFeed() {
  // A log region: new replies are announced politely without moving focus.
  const element = h('div', {
    className: 'chat-messages',
    role: 'log',
    'aria-live': 'polite',
    'aria-relevant': 'additions',
    'aria-label': t('chat.conversation'),
  });

  // Shown once the person has scrolled up. Sticky inside the scrolling feed, so
  // it rides the bottom edge; says how many messages arrived while away.
  const jumpLabel = h('span', {}, t('chat.jump'));
  const jumpBtn = h('button', {
    className: 'chat-jump',
    type: 'button',
    onClick: () => { unread = 0; scrollToBottom(true); },
  }, [h('span', { innerHTML: icon('chevron-down') }), jumpLabel]);
  jumpBtn.style.display = 'none';
  element.appendChild(jumpBtn);

  // Engine "thinking" indicator. Always present in the DOM, hidden by default.
  // chat.js shows/hides it based on engine activity so the feed visibly
  // pulses whenever the engine is mid-stage but hasn't emitted an event in a
  // while, without this the chat looks frozen during LLM round-trips.
  // The label carries REAL status only - "Building: <task>", "Building 3 tasks
  // in parallel…" - set by brain-events.js. It is empty the rest of the time:
  // the animated dots already say "working", and filling the gap with rotating
  // copy just gave the eye something to read that carried no information.
  const brainTypingLabel = h('span', { className: 'brain-typing__label' }, '');
  const brainTyping = h('div', { className: 'brain-typing' }, [
    createAvatar('Open Fabrica', 'brain'),
    h('div', { className: 'typing-indicator' }, [
      h('span', { className: 'typing-dot' }),
      h('span', { className: 'typing-dot' }),
      h('span', { className: 'typing-dot' }),
    ]),
    brainTypingLabel,
  ]);
  brainTyping.style.display = 'none';

  let autoScroll = true;
  let unread = 0;

  function syncJump() {
    jumpBtn.style.display = autoScroll ? 'none' : '';
    jumpLabel.textContent = unread > 0 ? tn('chat.newMessages', unread) : t('chat.jump');
  }

  // Track scroll position to decide auto-scroll
  element.addEventListener('scroll', () => {
    const { scrollTop, scrollHeight, clientHeight } = element;
    autoScroll = scrollHeight - scrollTop - clientHeight < 100;
    if (autoScroll) unread = 0;
    syncJump();
  });

  function scrollToBottom(force = false) {
    if (autoScroll || force) {
      requestAnimationFrame(() => {
        element.scrollTop = element.scrollHeight;
      });
    }
  }

  function append(messageEl) {
    messageEl.classList.add('chat-enter');
    // Insert before the typing indicator if it's currently parented
    // here, so "Open Fabrica is working…" always sits at the bottom of the feed.
    // The jump button is always last.
    if (brainTyping.parentNode === element) {
      element.insertBefore(messageEl, brainTyping);
    } else {
      element.insertBefore(messageEl, jumpBtn);
    }
    if (!autoScroll) {
      unread += 1;
      syncJump();
    }
    // Trigger animation after paint
    requestAnimationFrame(() => {
      requestAnimationFrame(() => {
        messageEl.classList.remove('chat-enter');
      });
    });
    scrollToBottom();
  }

  function clearFeed() {
    clear(element);
    unread = 0;
    element.appendChild(jumpBtn);
    syncJump();
  }

  function showBrainTyping() {
    if (brainTyping.parentNode !== element) {
      element.insertBefore(brainTyping, jumpBtn);
    }
    brainTyping.style.display = '';
    scrollToBottom();
  }

  function hideBrainTyping() {
    brainTyping.style.display = 'none';
    // Clear the label so the next show() doesn't inherit stale status.
    brainTypingLabel.textContent = '';
  }

  // Update the inline label without re-creating the indicator. A non-empty
  // string shows a context line ("Building: home page"); an empty string
  // clears it and leaves just the dots.
  function setBrainTypingLabel(text) {
    brainTypingLabel.textContent = text || '';
  }

  return {
    element,
    append,
    clear: clearFeed,
    scrollToBottom,
    showBrainTyping,
    hideBrainTyping,
    setBrainTypingLabel,
  };
}
