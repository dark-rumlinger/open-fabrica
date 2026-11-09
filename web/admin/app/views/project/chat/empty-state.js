import { h } from '../../../core/dom.js';
import { t } from '../../../core/i18n.js';
import { icon } from '../../../ui/icon.js';
import { greeting, displayName } from '../../../core/state.js';

// Lines cycled under the logo while the project is starting up. The setup and
// naming step is signal-less (no per-step events reach the UI), so a self-paced
// rotation is what conveys "something is happening" before the first real
// message lands and replaces this card.
const STARTING_LINES = [
  t('chat.start1'),
  t('chat.start2'),
  t('chat.start3'),
  t('chat.start4'),
];
const LINE_INTERVAL_MS = 2600;

/**
 * Manages the chat-empty card. The card lives in the feed until the first
 * message/event arrives, then is removed for good. It leads with the brand mark
 * under a soft accent glow plus a rotating status line so a brand-new project
 * reads as actively warming up rather than a static welcome screen.
 */
export function createEmptyStateController(feed) {
  let el = null;
  let timer = null;

  const reduce = window.matchMedia
    && window.matchMedia('(prefers-reduced-motion: reduce)').matches;

  function show() {
    if (el) return;
    const name = displayName();
    const title = name ? t('chat.greetingNamed', { greeting: greeting(), name }) : `${greeting()}`;

    const mark = h('div', { className: 'chat-empty__mark' }, [
      h('img', {
        className: 'chat-empty__logo',
        src: '/openfabrica_logo.svg',
        alt: 'Open Fabrica',
      }),
    ]);

    const statusText = h('span', { className: 'chat-empty__status-text is-in' }, STARTING_LINES[0]);
    const status = h('div', { className: 'chat-empty__status' }, [
      h('span', { className: 'chat-empty__status-dot' }),
      statusText,
    ]);

    el = h('div', { className: 'chat-empty' }, [
      mark,
      h('h3', { className: 'chat-empty__title' }, title),
      status,
      h('p', { className: 'chat-empty__text' }, t('chat.intro')),
      h('div', { className: 'chat-empty__hints' }, [
        h('div', { className: 'chat-empty__hint' }, [
          h('span', { innerHTML: icon('message-circle'), className: 'chat-empty__hint-icon' }),
          h('span', {}, t('chat.hint1')),
        ]),
        h('div', { className: 'chat-empty__hint' }, [
          h('span', { innerHTML: icon('help-circle'), className: 'chat-empty__hint-icon' }),
          h('span', {}, t('chat.hint2')),
        ]),
        h('div', { className: 'chat-empty__hint' }, [
          h('span', { innerHTML: icon('activity'), className: 'chat-empty__hint-icon' }),
          h('span', {}, t('chat.hint3')),
        ]),
      ]),
    ]);
    feed.append(el);

    // Cycle the status line on a timer. Reduced-motion users get the first line
    // held static (the rotation is decorative, not information they'd miss).
    if (!reduce) {
      let idx = 0;
      timer = setInterval(() => {
        idx = (idx + 1) % STARTING_LINES.length;
        statusText.textContent = STARTING_LINES[idx];
        // Restart the fade-in by replaying the animation class.
        statusText.classList.remove('is-in');
        void statusText.offsetWidth;
        statusText.classList.add('is-in');
      }, LINE_INTERVAL_MS);
    }
  }

  function remove() {
    if (timer) {
      clearInterval(timer);
      timer = null;
    }
    if (!el) return;
    const node = el;
    el = null; // guard against a second remove() racing the exit animation

    if (reduce) {
      node.remove();
      return;
    }

    // Fade and collapse out instead of snapping away: lock the current height,
    // then transition it (and opacity/padding) to zero so the first real
    // message slides up into the freed space rather than jumping.
    node.style.height = node.offsetHeight + 'px';
    void node.offsetWidth; // reflow so the locked height is the transition start
    node.classList.add('chat-empty--leaving');
    node.style.height = '0px';

    let done = false;
    const finish = () => { if (done) return; done = true; node.remove(); };
    node.addEventListener('transitionend', (e) => {
      if (e.propertyName === 'height' || e.propertyName === 'opacity') finish();
    });
    setTimeout(finish, 500); // safety net if transitionend is missed
  }

  return { show, remove };
}
