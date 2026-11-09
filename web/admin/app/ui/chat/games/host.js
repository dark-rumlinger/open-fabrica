/**
 * Game host: after the engine has worked continuously for a while, drops a
 * "fancy a game?" chip in the chat feed; picking a game mounts it in a shell
 * with a switcher strip. Driven by the typing manager (typing.js):
 * onActivity() while the engine is alive, onIdle() at LIVE. Deliberately
 * non-blocking: never steals the chat input, and short actions never reach
 * the threshold.
 */

import { h } from '../../../core/dom.js';
import { t } from '../../../core/i18n.js';
import { icon } from '../../icon.js';
import { GAMES } from './registry.js';

// How long a single action must run before we offer a game. Long enough that
// ordinary turns never trigger it; short enough to land before real boredom.
const INVITE_DELAY_MS = 25000;

/**
 * @param {{ element: HTMLElement, scrollToBottom: (force?: boolean) => void }} feed
 * @param {{ add: (fn: () => void) => void }} [scope]
 */
export function createGameHost(feed, scope) {
  let inviteEl = null;
  let shellEl = null;      // wraps the switcher strip + the active game
  let game = null;         // currently mounted game instance
  let activeId = null;
  let switcherTabs = [];   // [{ id, el }]
  let timer = null;
  // Suppressed until the next idle, so a dismissed/closed game doesn't re-nag
  // during the same long action.
  let suppressed = false;

  function armed() {
    return !inviteEl && !shellEl && !suppressed && timer == null;
  }

  function onActivity() {
    if (!armed()) return;
    timer = setTimeout(showInvite, INVITE_DELAY_MS);
  }

  function onIdle() {
    clearTimer();
    removeInvite();
    suppressed = false;
  }

  function clearTimer() {
    if (timer != null) { clearTimeout(timer); timer = null; }
  }

  // --- Invite chip -------------------------------------------------------

  function showInvite() {
    timer = null;
    if (inviteEl || shellEl) return;

    const picker = h('div', { className: 'chat-game-invite__games' },
      GAMES.map((g) => h('button', {
        className: 'chat-game-invite__game',
        title: t('game.play', { name: g.name }),
        onClick: () => openGame(g),
      }, [
        h('span', { className: 'chat-game-invite__game-icon', innerHTML: icon(g.icon) }),
        g.name,
      ])),
    );

    inviteEl = h('div', { className: 'chat-game-invite' }, [
      h('span', { className: 'chat-game-invite__icon', innerHTML: icon('gamepad') }),
      h('span', { className: 'chat-game-invite__text' }, t('game.invite')),
      picker,
      h('button', {
        className: 'chat-game-invite__dismiss',
        title: t('game.noThanks'), 'aria-label': t('common.dismiss'),
        innerHTML: icon('x'),
        onClick: () => { removeInvite(); suppressed = true; },
      }),
    ]);
    feed.element.appendChild(inviteEl);
    feed.scrollToBottom(true);
  }

  function removeInvite() {
    if (inviteEl) { inviteEl.remove(); inviteEl = null; }
  }

  // --- Game shell + switcher --------------------------------------------

  function openGame(def) {
    removeInvite();
    if (shellEl) { switchTo(def); return; }

    switcherTabs = GAMES.map((g) => {
      const el = h('button', {
        className: 'chat-game-switcher__tab',
        title: t('game.play', { name: g.name }), 'aria-label': g.name,
        innerHTML: icon(g.icon),
        onClick: () => switchTo(g),
      });
      return { id: g.id, el };
    });
    const switcher = h('div', { className: 'chat-game-switcher' }, switcherTabs.map((tab) => tab.el));

    shellEl = h('div', { className: 'chat-game-shell' }, [switcher]);
    feed.element.appendChild(shellEl);
    activate(def);
    feed.scrollToBottom(true);
  }

  // Swap the mounted game in place, keeping the shell and the player's spot.
  function activate(def) {
    if (game) { game.destroy(); game = null; }
    activeId = def.id;
    game = def.create({ onClose: closeGame });
    shellEl.appendChild(game.element);
    for (const tab of switcherTabs) {
      tab.el.classList.toggle('chat-game-switcher__tab--active', tab.id === activeId);
    }
    game.focus();
    feed.scrollToBottom(true);
  }

  function switchTo(def) {
    if (!shellEl) { openGame(def); return; }
    if (def.id === activeId) { if (game) game.focus(); return; }
    activate(def);
  }

  function closeGame() {
    // The game tears down its own element on close; we remove the shell and
    // avoid immediately re-offering until the next idle->work cycle.
    if (game) { game.destroy(); game = null; }
    if (shellEl) { shellEl.remove(); shellEl = null; }
    switcherTabs = [];
    activeId = null;
    suppressed = true;
  }

  function destroy() {
    clearTimer();
    removeInvite();
    if (game) { game.destroy(); game = null; }
    if (shellEl) { shellEl.remove(); shellEl = null; }
    switcherTabs = [];
    activeId = null;
  }

  if (scope) scope.add(destroy);

  return { onActivity, onIdle, destroy };
}
