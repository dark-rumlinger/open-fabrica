/**
 * Self-contained Minesweeper for the chat feed. Same contract as the other
 * games: createMinesweeper({ onClose }) -> { element, focus, destroy }.
 * First click is always safe: mines are placed after it, excluding the
 * clicked cell.
 */

import { h } from '../../../core/dom.js';
import { t } from '../../../core/i18n.js';
import { icon } from '../../icon.js';

export const meta = { id: 'minesweeper', name: t('game.name.minesweeper'), icon: 'bomb' };

const COLS = 9;
const ROWS = 9;
const MINES = 10;

/**
 * @param {{ onClose?: () => void }} [opts]
 * @returns {{ element: HTMLElement, focus: () => void, destroy: () => void }}
 */
export function createMinesweeper(opts = {}) {
  const N = COLS * ROWS;
  let mine, revealed, flag, count; // parallel arrays over N cells
  let placed, over, won, flags, explodedIdx, flagMode;

  function reset() {
    mine = new Array(N).fill(false);
    revealed = new Array(N).fill(false);
    flag = new Array(N).fill(false);
    count = new Array(N).fill(0);
    placed = false;
    over = false;
    won = false;
    flags = 0;
    explodedIdx = -1;
    flagMode = false;
  }

  const rc = (i) => [Math.floor(i / COLS), i % COLS];
  function neighbors(i) {
    const [r, c] = rc(i);
    const out = [];
    for (let dr = -1; dr <= 1; dr++)
      for (let dc = -1; dc <= 1; dc++) {
        if (!dr && !dc) continue;
        const nr = r + dr, nc = c + dc;
        if (nr >= 0 && nr < ROWS && nc >= 0 && nc < COLS) out.push(nr * COLS + nc);
      }
    return out;
  }

  function placeMines(safe) {
    const candidates = [];
    for (let i = 0; i < N; i++) if (i !== safe) candidates.push(i);
    for (let i = candidates.length - 1; i > 0; i--) {
      const j = Math.floor(Math.random() * (i + 1));
      [candidates[i], candidates[j]] = [candidates[j], candidates[i]];
    }
    for (let k = 0; k < MINES; k++) mine[candidates[k]] = true;
    for (let i = 0; i < N; i++) count[i] = neighbors(i).filter((n) => mine[n]).length;
    placed = true;
  }

  function reveal(i) {
    if (over || revealed[i] || flag[i]) return;
    if (!placed) placeMines(i);

    if (mine[i]) { explodedIdx = i; lose(); return; }

    // Flood-fill outward from any zero-count cell.
    const stack = [i];
    while (stack.length) {
      const j = stack.pop();
      if (revealed[j] || flag[j]) continue;
      revealed[j] = true;
      if (count[j] === 0) for (const n of neighbors(j)) if (!revealed[n] && !mine[n]) stack.push(n);
    }
    checkWin();
    render();
  }

  function toggleFlag(i) {
    if (over || revealed[i]) return;
    flag[i] = !flag[i];
    flags += flag[i] ? 1 : -1;
    render();
  }

  function lose() {
    over = true;
    for (let i = 0; i < N; i++) if (mine[i]) revealed[i] = true;
    render();
  }

  function checkWin() {
    for (let i = 0; i < N; i++) if (!mine[i] && !revealed[i]) return;
    over = true;
    won = true;
  }

  function restart() {
    reset();
    render();
    element.focus();
  }

  // --- DOM ---------------------------------------------------------------

  const minesEl = h('b', {}, String(MINES));
  const overlayEl = h('div', { className: 'chat-game__overlay' });

  const boardEl = h('div', {
    className: 'chat-game__board chat-game__board--mines',
    style: { gridTemplateColumns: `repeat(${COLS}, 1fr)` },
  });
  const cells = [];
  for (let i = 0; i < N; i++) {
    const idx = i;
    const cell = h('button', {
      className: 'mine-cell', type: 'button', 'aria-label': t('game.cell'),
      onClick: () => (flagMode ? toggleFlag(idx) : reveal(idx)),
    });
    cells.push(cell);
    boardEl.appendChild(cell);
  }
  // Right-click flags regardless of flag-mode (desktop convenience).
  boardEl.addEventListener('contextmenu', onContextMenu);

  const title = h('span', { className: 'chat-game__title' }, [
    h('span', { className: 'chat-game__title-icon', innerHTML: icon('bomb') }),
    meta.name,
  ]);
  const flagBtn = h('button', {
    className: 'chat-game__icon-btn', title: t('game.flagMode'), 'aria-label': t('game.toggleFlag'),
    innerHTML: icon('flag'), onClick: toggleFlagMode,
  });
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
      h('div', { className: 'chat-game__score' }, [h('span', {}, t('game.mines')), minesEl]),
    ]),
    h('div', { className: 'chat-game__actions' }, [flagBtn, restartBtn, closeBtn]),
  ]);

  const stage = h('div', { className: 'chat-game__stage' }, [boardEl, overlayEl]);
  const hint = h('div', { className: 'chat-game__hint' }, t('game.hint.minesweeper'));
  const element = h('div', { className: 'chat-game', tabindex: '0' }, [header, stage, hint]);

  function onContextMenu(e) {
    e.preventDefault();
    const btn = e.target.closest('.mine-cell');
    if (!btn) return;
    const i = cells.indexOf(btn);
    if (i >= 0) toggleFlag(i);
  }

  function toggleFlagMode() {
    flagMode = !flagMode;
    flagBtn.classList.toggle('chat-game__icon-btn--active', flagMode);
    element.focus();
  }

  function render() {
    for (let i = 0; i < N; i++) {
      const cell = cells[i];
      if (flag[i] && !revealed[i]) {
        cell.className = 'mine-cell mine-cell--flag';
        cell.innerHTML = icon('flag');
      } else if (!revealed[i]) {
        cell.className = 'mine-cell';
        cell.textContent = '';
      } else if (mine[i]) {
        cell.className = `mine-cell mine-cell--revealed mine-cell--mine${i === explodedIdx ? ' mine-cell--exploded' : ''}`;
        cell.innerHTML = icon('bomb');
      } else {
        const c = count[i];
        cell.className = `mine-cell mine-cell--revealed${c ? ` mine-cell--n${c}` : ''}`;
        cell.textContent = c ? String(c) : '';
      }
    }
    minesEl.textContent = String(Math.max(0, MINES - flags));

    if (over) {
      overlayEl.style.display = '';
      overlayEl.replaceChildren(
        h('div', { className: 'chat-game__overlay-title' }, won ? t('game.cleared') : t('game.boom')),
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
    boardEl.removeEventListener('contextmenu', onContextMenu);
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
