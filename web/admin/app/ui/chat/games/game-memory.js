/**
 * A self-contained Memory (concentration) game for the chat feed. Same contract
 * as the other games: createMemory({ onClose }) -> { element, focus, destroy }.
 * Card faces are SVGs from the shared icon set (no emoji), so it needs zero new
 * art. Click / tap to flip; matched pairs lock; fewest moves persists as best.
 */

import { h } from '../../../core/dom.js';
import { t, tn } from '../../../core/i18n.js';
import { icon } from '../../icon.js';
import { readBest, writeBest } from './registry.js';

export const meta = { id: 'memory', name: t('game.name.memory'), icon: 'copy' };

// Six pairs -> 12 cards in a 4x3 grid. Faces are existing icon.js keys.
const FACES = ['star', 'zap', 'globe', 'home', 'shield', 'target'];
const COLS = 4;
const BEST_KEY = 'of:gameMemory:best';
const FLIP_BACK_MS = 800;

function shuffle(arr) {
  const a = arr.slice();
  for (let i = a.length - 1; i > 0; i--) {
    const j = Math.floor(Math.random() * (i + 1));
    [a[i], a[j]] = [a[j], a[i]];
  }
  return a;
}

/**
 * @param {{ onClose?: () => void }} [opts]
 * @returns {{ element: HTMLElement, focus: () => void, destroy: () => void }}
 */
export function createMemory(opts = {}) {
  const PAIRS = FACES.length;
  let deck;            // [{ face }]
  let status;          // 0 down, 1 up, 2 matched
  let firstIdx, moves, matched, lock;
  let best = readBest(BEST_KEY);
  let flipTimer = null;

  function reset() {
    deck = shuffle([...FACES, ...FACES]).map((face) => ({ face }));
    status = deck.map(() => 0);
    firstIdx = null;
    moves = 0;
    matched = 0;
    lock = false;
    if (flipTimer != null) { clearTimeout(flipTimer); flipTimer = null; }
  }

  function flip(i) {
    if (lock || status[i] !== 0) return;
    status[i] = 1;

    if (firstIdx == null) {
      firstIdx = i;
      render();
      return;
    }

    moves++;
    if (deck[i].face === deck[firstIdx].face) {
      status[i] = 2;
      status[firstIdx] = 2;
      firstIdx = null;
      matched++;
      render();
      if (matched === PAIRS) win();
    } else {
      lock = true;
      const a = firstIdx, b = i;
      firstIdx = null;
      render();
      flipTimer = setTimeout(() => {
        flipTimer = null;
        status[a] = 0;
        status[b] = 0;
        lock = false;
        render();
      }, FLIP_BACK_MS);
    }
  }

  function win() {
    if (best === 0 || moves < best) { best = moves; writeBest(BEST_KEY, best); }
    render();
  }

  function restart() {
    reset();
    render();
    element.focus();
  }

  const won = () => matched === PAIRS;

  // --- DOM ---------------------------------------------------------------

  const movesEl = h('b', {}, '0');
  const bestEl = h('b', {}, best ? String(best) : '·');
  const overlayEl = h('div', { className: 'chat-game__overlay' });

  const boardEl = h('div', {
    className: 'chat-game__board chat-game__board--memory',
    style: { gridTemplateColumns: `repeat(${COLS}, 1fr)` },
  });
  const cards = [];
  for (let i = 0; i < FACES.length * 2; i++) {
    const idx = i;
    const card = h('button', {
      className: 'memory-card', type: 'button',
      'aria-label': t('game.card'), onClick: () => flip(idx),
    });
    cards.push(card);
    boardEl.appendChild(card);
  }

  const title = h('span', { className: 'chat-game__title' }, [
    h('span', { className: 'chat-game__title-icon', innerHTML: icon('copy') }),
    meta.name,
  ]);
  const restartBtn = h('button', {
    className: 'chat-game__icon-btn', title: t('game.newGame'), 'aria-label': t('game.newGame'),
    innerHTML: icon('rotate-ccw'), onClick: restart,
  });
  const closeBtn = h('button', {
    className: 'chat-game__icon-btn', title: t('game.closeGame'), 'aria-label': t('game.closeGame'),
    innerHTML: icon('x'),
    onClick: () => { teardown(); if (opts.onClose) opts.onClose(); },
  });

  const header = h('div', { className: 'chat-game__header' }, [
    title,
    h('div', { className: 'chat-game__scores' }, [
      h('div', { className: 'chat-game__score' }, [h('span', {}, t('game.moves')), movesEl]),
      h('div', { className: 'chat-game__score' }, [h('span', {}, t('game.best')), bestEl]),
    ]),
    h('div', { className: 'chat-game__actions' }, [restartBtn, closeBtn]),
  ]);

  const stage = h('div', { className: 'chat-game__stage' }, [boardEl, overlayEl]);
  const hint = h('div', { className: 'chat-game__hint' }, t('game.hint.memory'));
  const element = h('div', { className: 'chat-game', tabindex: '0' }, [header, stage, hint]);

  function render() {
    for (let i = 0; i < cards.length; i++) {
      const st = status[i];
      const cls = st === 2 ? 'memory-card memory-card--matched'
        : st === 1 ? 'memory-card memory-card--up'
          : 'memory-card memory-card--down';
      cards[i].className = cls;
      // Face SVG stays in the DOM; CSS toggles its visibility by state.
      cards[i].innerHTML = `<span class="memory-card__face">${icon(deck[i].face)}</span>`;
    }
    movesEl.textContent = String(moves);
    bestEl.textContent = best ? String(best) : '·';

    if (won()) {
      overlayEl.style.display = '';
      overlayEl.replaceChildren(
        h('div', { className: 'chat-game__overlay-title' }, tn('game.solvedIn', moves)),
        h('button', { className: 'chat-game__overlay-btn', onClick: restart }, t('game.playAgain')),
      );
    } else {
      overlayEl.style.display = 'none';
      overlayEl.replaceChildren();
    }
  }

  // Idempotent so the close button and host cleanup can both call it safely.
  let destroyed = false;
  function teardown() {
    if (destroyed) return;
    destroyed = true;
    if (flipTimer != null) { clearTimeout(flipTimer); flipTimer = null; }
    element.remove();
  }

  reset();
  render();

  return {
    element,
    focus: () => element.focus(),
    destroy: teardown,
  };
}
