/**
 * Self-contained Snake for the chat feed. Same contract as 2048:
 * createSnake({ onClose }) -> { element, focus, destroy }. Movement is a
 * self-rescheduling setTimeout chain (speed ramps with score, stays correct
 * under prefers-reduced-motion) rather than a CSS animation.
 */

import { h } from '../../../core/dom.js';
import { t } from '../../../core/i18n.js';
import { icon } from '../../icon.js';
import { readBest, writeBest } from './registry.js';

export const meta = { id: 'snake', name: t('game.name.snake'), icon: 'snake' };

const COLS = 15;
const ROWS = 13;
const BEST_KEY = 'of:gameSnake:best';
const BASE_SPEED_MS = 150;
const MIN_SPEED_MS = 75;
const SWIPE_THRESHOLD = 24;

const DIRS = {
  ArrowLeft: { x: -1, y: 0 }, ArrowRight: { x: 1, y: 0 },
  ArrowUp: { x: 0, y: -1 }, ArrowDown: { x: 0, y: 1 },
  a: { x: -1, y: 0 }, d: { x: 1, y: 0 }, w: { x: 0, y: -1 }, s: { x: 0, y: 1 },
};

/**
 * @param {{ onClose?: () => void }} [opts]
 * @returns {{ element: HTMLElement, focus: () => void, destroy: () => void }}
 */
export function createSnake(opts = {}) {
  let snake, dir, pendingDir, food, score, best = readBest(BEST_KEY);
  let running = false, started = false, over = false;
  let loopId = null;

  function reset() {
    const cy = Math.floor(ROWS / 2);
    const cx = Math.floor(COLS / 2);
    snake = [{ x: cx, y: cy }, { x: cx - 1, y: cy }, { x: cx - 2, y: cy }];
    dir = { x: 1, y: 0 };
    pendingDir = null;
    score = 0;
    running = false;
    started = false;
    over = false;
    placeFood();
  }

  function placeFood() {
    const free = [];
    for (let y = 0; y < ROWS; y++)
      for (let x = 0; x < COLS; x++)
        if (!snake.some((s) => s.x === x && s.y === y)) free.push({ x, y });
    food = free.length ? free[Math.floor(Math.random() * free.length)] : null;
  }

  function speedMs() {
    return Math.max(MIN_SPEED_MS, BASE_SPEED_MS - score * 4);
  }

  function scheduleTick() {
    loopId = setTimeout(() => { tick(); if (running) scheduleTick(); }, speedMs());
  }

  function start() {
    if (running || over) return;
    running = true;
    started = true;
    scheduleTick();
    render();
  }

  function stopLoop() {
    if (loopId != null) { clearTimeout(loopId); loopId = null; }
  }

  function togglePause() {
    if (over) return;
    if (running) { running = false; stopLoop(); render(); }
    else start();
  }

  function setDir(nd) {
    const cur = pendingDir || dir;
    if (nd.x === -cur.x && nd.y === -cur.y) return; // can't reverse onto self
    pendingDir = nd;
    if (!running && !over) start();
  }

  function tick() {
    if (pendingDir) { dir = pendingDir; pendingDir = null; }
    const head = { x: snake[0].x + dir.x, y: snake[0].y + dir.y };

    if (head.x < 0 || head.x >= COLS || head.y < 0 || head.y >= ROWS) return gameOver();

    const willGrow = food && head.x === food.x && head.y === food.y;
    const body = willGrow ? snake : snake.slice(0, -1); // tail vacates unless growing
    if (body.some((s) => s.x === head.x && s.y === head.y)) return gameOver();

    snake.unshift(head);
    if (willGrow) {
      score++;
      if (score > best) { best = score; writeBest(BEST_KEY, best); }
      placeFood();
      if (!food) return gameOver(); // board full, a win treated as the end
    } else {
      snake.pop();
    }
    render();
  }

  function gameOver() {
    over = true;
    running = false;
    stopLoop();
    render();
  }

  function restart() {
    stopLoop();
    reset();
    render();
    element.focus();
  }

  // --- DOM ---------------------------------------------------------------

  const scoreEl = h('b', {}, '0');
  const bestEl = h('b', {}, String(best));
  const overlayEl = h('div', { className: 'chat-game__overlay' });

  const boardEl = h('div', {
    className: 'chat-game__board chat-game__board--snake chat-game__board--swipe',
    style: { gridTemplateColumns: `repeat(${COLS}, 1fr)` },
  });
  const cells = [];
  for (let i = 0; i < COLS * ROWS; i++) {
    const cell = h('div', { className: 'snake-cell' });
    cells.push(cell);
    boardEl.appendChild(cell);
  }

  const title = h('span', { className: 'chat-game__title' }, [
    h('span', { className: 'chat-game__title-icon', innerHTML: icon('snake') }),
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
      h('div', { className: 'chat-game__score' }, [h('span', {}, t('game.score')), scoreEl]),
      h('div', { className: 'chat-game__score' }, [h('span', {}, t('game.best')), bestEl]),
    ]),
    h('div', { className: 'chat-game__actions' }, [restartBtn, closeBtn]),
  ]);

  const stage = h('div', { className: 'chat-game__stage' }, [boardEl, overlayEl]);
  const hint = h('div', { className: 'chat-game__hint' }, t('game.hint.snake'));
  const element = h('div', { className: 'chat-game', tabindex: '0' }, [header, stage, hint]);

  function render() {
    for (let i = 0; i < cells.length; i++) cells[i].className = 'snake-cell';
    if (food) cells[food.y * COLS + food.x].classList.add('snake-cell--food');
    snake.forEach((s, idx) => {
      const cell = cells[s.y * COLS + s.x];
      cell.classList.add('snake-cell--snake');
      if (idx === 0) cell.classList.add('snake-cell--head');
    });

    scoreEl.textContent = String(score);
    bestEl.textContent = String(best);

    const showOverlay = over || !running;
    if (!showOverlay) { overlayEl.style.display = 'none'; overlayEl.replaceChildren(); return; }

    overlayEl.style.display = '';
    let titleText, btnText, btnAction;
    if (over) {
      titleText = t('game.gameOverScore', { score });
      btnText = t('game.playAgain');
      btnAction = restart;
    } else if (!started) {
      titleText = meta.name;
      btnText = t('game.start');
      btnAction = start;
    } else {
      titleText = t('game.paused');
      btnText = t('game.resume');
      btnAction = start;
    }
    overlayEl.replaceChildren(
      h('div', { className: 'chat-game__overlay-title' }, titleText),
      h('button', { className: 'chat-game__overlay-btn', onClick: () => { btnAction(); element.focus(); } }, btnText),
    );
  }

  // --- Input -------------------------------------------------------------

  function onKeyDown(e) {
    if (e.key === ' ' || e.key === 'Spacebar') { e.preventDefault(); togglePause(); return; }
    const nd = DIRS[e.key];
    if (!nd) return;
    e.preventDefault();
    setDir(nd);
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
    if (Math.abs(dx) > Math.abs(dy)) setDir(dx > 0 ? DIRS.ArrowRight : DIRS.ArrowLeft);
    else setDir(dy > 0 ? DIRS.ArrowDown : DIRS.ArrowUp);
  }
  boardEl.addEventListener('touchstart', onTouchStart, { passive: true });
  boardEl.addEventListener('touchend', onTouchEnd, { passive: true });

  // Idempotent so the close button and host cleanup can both call it safely.
  let destroyed = false;
  function teardown() {
    if (destroyed) return;
    destroyed = true;
    stopLoop();
    element.removeEventListener('keydown', onKeyDown);
    boardEl.removeEventListener('touchstart', onTouchStart);
    boardEl.removeEventListener('touchend', onTouchEnd);
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
