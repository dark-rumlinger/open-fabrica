/**
 * Self-contained 2048 for the chat feed, shown during long waits. Keyboard
 * (arrows/WASD) + touch-swipe; best score persists in localStorage. Keys are
 * handled on the game element itself (tabindex=0, focused on mount) so the
 * chat input keeps its own arrow-key behavior when focused.
 */

import { h, clear } from '../../../core/dom.js';
import { t } from '../../../core/i18n.js';
import { icon } from '../../icon.js';
import { readBest, writeBest } from './registry.js';

/** Registry metadata. id is stable, icon is an icon.js key. */
export const meta = { id: '2048', name: '2048', icon: 'grid' };

const SIZE = 4;
const BEST_KEY = 'of:game2048:best';
const SWIPE_THRESHOLD = 24; // px before a touch drag counts as a move

/**
 * Slide + merge a single row toward index 0. Returns the new row and the score
 * gained from merges this pass.
 */
function slideRow(row) {
  const packed = row.filter((v) => v !== 0);
  let gained = 0;
  for (let i = 0; i < packed.length - 1; i++) {
    if (packed[i] === packed[i + 1]) {
      packed[i] *= 2;
      gained += packed[i];
      packed.splice(i + 1, 1);
    }
  }
  while (packed.length < SIZE) packed.push(0);
  return { row: packed, gained };
}

/**
 * Read the four lines of the board in the order a given direction collapses
 * them, so every move reduces to "slide each line toward index 0". Returns a
 * list of {cells: [{r,c}], values: []} describing each line.
 */
function linesFor(board, dir) {
  const lines = [];
  for (let i = 0; i < SIZE; i++) {
    const cells = [];
    for (let j = 0; j < SIZE; j++) {
      let r, c;
      if (dir === 'left') { r = i; c = j; }
      else if (dir === 'right') { r = i; c = SIZE - 1 - j; }
      else if (dir === 'up') { r = j; c = i; }
      else { r = SIZE - 1 - j; c = i; } // down
      cells.push({ r, c });
    }
    lines.push({ cells, values: cells.map(({ r, c }) => board[r][c]) });
  }
  return lines;
}

/**
 * @param {{ onClose?: () => void }} [opts]
 * @returns {{ element: HTMLElement, focus: () => void, destroy: () => void }}
 */
export function createGame2048(opts = {}) {
  let board = emptyBoard();
  let score = 0;
  let best = readBest(BEST_KEY);
  let over = false;
  let won = false; // latched so the win banner only nags once

  function emptyBoard() {
    return Array.from({ length: SIZE }, () => Array(SIZE).fill(0));
  }

  function emptyCells() {
    const cells = [];
    for (let r = 0; r < SIZE; r++)
      for (let c = 0; c < SIZE; c++)
        if (board[r][c] === 0) cells.push({ r, c });
    return cells;
  }

  function spawn() {
    const cells = emptyCells();
    if (!cells.length) return;
    const { r, c } = cells[Math.floor(Math.random() * cells.length)];
    board[r][c] = Math.random() < 0.9 ? 2 : 4;
    spawnedKey = `${r}-${c}`; // tag for the pop-in animation on next render
  }

  function canMove() {
    if (emptyCells().length) return true;
    for (let r = 0; r < SIZE; r++)
      for (let c = 0; c < SIZE; c++) {
        const v = board[r][c];
        if ((c < SIZE - 1 && board[r][c + 1] === v) || (r < SIZE - 1 && board[r + 1][c] === v)) return true;
      }
    return false;
  }

  function move(dir) {
    if (over) return;
    let moved = false;
    let gainedTotal = 0;
    for (const { cells, values } of linesFor(board, dir)) {
      const { row, gained } = slideRow(values);
      gainedTotal += gained;
      cells.forEach(({ r, c }, idx) => {
        if (board[r][c] !== row[idx]) moved = true;
        board[r][c] = row[idx];
      });
    }
    if (!moved) return;

    score += gainedTotal;
    if (score > best) { best = score; writeBest(BEST_KEY, best); }
    if (!won && board.some((row) => row.some((v) => v >= 2048))) won = true;

    spawn();
    if (!canMove()) over = true;
    render();
  }

  function restart() {
    board = emptyBoard();
    score = 0;
    over = false;
    won = false;
    spawn();
    spawn();
    render();
    element.focus();
  }

  // --- DOM ---------------------------------------------------------------

  let spawnedKey = null;

  const scoreEl = h('b', {}, '0');
  const bestEl = h('b', {}, String(best));
  const boardEl = h('div', { className: 'chat-game__board chat-game__board--swipe' });
  const overlayEl = h('div', { className: 'chat-game__overlay' });
  overlayEl.style.display = 'none';

  const title = h('span', { className: 'chat-game__title' }, [
    h('span', { className: 'chat-game__title-icon', innerHTML: icon('gamepad') }),
    '2048',
  ]);

  const restartBtn = h('button', {
    className: 'chat-game__icon-btn',
    title: t('game.newGame'),
    'aria-label': t('game.newGame'),
    innerHTML: icon('rotate-ccw'),
    onClick: restart,
  });
  const closeBtn = h('button', {
    className: 'chat-game__icon-btn',
    title: t('game.closeGame'),
    'aria-label': t('game.closeGame'),
    innerHTML: icon('x'),
    // Tear down our own element first so close always works, then notify the
    // host so it can update its state (re-arm suppression etc.).
    onClick: () => { teardown(); if (opts.onClose) opts.onClose(); },
  });

  const header = h('div', { className: 'chat-game__header' }, [
    title,
    h('div', { className: 'chat-game__scores' }, [
      h('div', { className: 'chat-game__score' }, [h('span', {}, t('game.score')), scoreEl]),
      h('div', { className: 'chat-game__score' }, [h('span', {}, t('game.best')), bestEl]),
    ]),
    h('div', { className: 'chat-game__actions' }, [restartBtn, closeBtn]),
  ]);

  const stage = h('div', { className: 'chat-game__stage' }, [boardEl, overlayEl]);
  const hint = h('div', { className: 'chat-game__hint' }, t('game.hint.2048'));

  const element = h('div', { className: 'chat-game', tabindex: '0' }, [header, stage, hint]);

  function render() {
    clear(boardEl);
    for (let r = 0; r < SIZE; r++) {
      for (let c = 0; c < SIZE; c++) {
        const v = board[r][c];
        const tile = h('div', {
          className: `chat-game__tile${v ? ` chat-game__tile--${v <= 2048 ? v : 'super'}` : ' chat-game__tile--empty'}`,
        }, v ? String(v) : '');
        if (v && spawnedKey === `${r}-${c}`) tile.classList.add('chat-game__tile--spawn');
        boardEl.appendChild(tile);
      }
    }
    spawnedKey = null;

    scoreEl.textContent = String(score);
    bestEl.textContent = String(best);

    if (over || won) {
      overlayEl.style.display = '';
      clear(overlayEl);
      overlayEl.appendChild(h('div', { className: 'chat-game__overlay-title' }, over ? t('game.gameOver') : t('game.hit2048')));
      overlayEl.appendChild(h('button', {
        className: 'chat-game__overlay-btn',
        onClick: over ? restart : () => { won = false; overlayEl.style.display = 'none'; element.focus(); },
      }, over ? t('game.playAgain') : t('game.keepGoing')));
    } else {
      overlayEl.style.display = 'none';
    }
  }

  // --- Input -------------------------------------------------------------

  const KEYS = {
    ArrowLeft: 'left', ArrowRight: 'right', ArrowUp: 'up', ArrowDown: 'down',
    a: 'left', d: 'right', w: 'up', s: 'down',
  };
  function onKeyDown(e) {
    const dir = KEYS[e.key];
    if (!dir) return;
    e.preventDefault();
    move(dir);
  }
  element.addEventListener('keydown', onKeyDown);

  let touchStart = null;
  function onTouchStart(e) {
    const t = e.changedTouches[0];
    touchStart = { x: t.clientX, y: t.clientY };
  }
  function onTouchEnd(e) {
    if (!touchStart) return;
    const t = e.changedTouches[0];
    const dx = t.clientX - touchStart.x;
    const dy = t.clientY - touchStart.y;
    touchStart = null;
    if (Math.max(Math.abs(dx), Math.abs(dy)) < SWIPE_THRESHOLD) return;
    if (Math.abs(dx) > Math.abs(dy)) move(dx > 0 ? 'right' : 'left');
    else move(dy > 0 ? 'down' : 'up');
  }
  boardEl.addEventListener('touchstart', onTouchStart, { passive: true });
  boardEl.addEventListener('touchend', onTouchEnd, { passive: true });

  // Idempotent so the close button and host cleanup can both call it safely.
  let destroyed = false;
  function teardown() {
    if (destroyed) return;
    destroyed = true;
    element.removeEventListener('keydown', onKeyDown);
    boardEl.removeEventListener('touchstart', onTouchStart);
    boardEl.removeEventListener('touchend', onTouchEnd);
    element.remove();
  }

  // Boot
  spawn();
  spawn();
  render();

  return {
    element,
    focus: () => element.focus(),
    destroy: teardown,
  };
}
