/**
 * Self-contained Simon for the chat feed. Same contract as the other games:
 * createSimon({ onClose }) -> { element, focus, destroy }. Flash timing runs
 * on tracked setTimeout ids so teardown cancels them cleanly; input is
 * ignored while the sequence is playing back.
 */

import { h } from '../../../core/dom.js';
import { t } from '../../../core/i18n.js';
import { icon } from '../../icon.js';
import { readBest, writeBest } from './registry.js';

export const meta = { id: 'simon', name: t('game.name.simon'), icon: 'grid-dots' };

const COLORS = ['#2faa9a', '#d6336c', '#1d9bf0', '#c9a227'];
const BEST_KEY = 'of:gameSimon:best';
const ON_MS = 380;
const GAP_MS = 200;

/**
 * @param {{ onClose?: () => void }} [opts]
 * @returns {{ element: HTMLElement, focus: () => void, destroy: () => void }}
 */
export function createSimon(opts = {}) {
  let sequence, inputIdx, state; // state: 'idle' | 'showing' | 'input' | 'over'
  let best = readBest(BEST_KEY);
  const timers = new Set();

  function later(fn, ms) {
    const id = setTimeout(() => { timers.delete(id); fn(); }, ms);
    timers.add(id);
    return id;
  }
  function clearTimers() {
    for (const id of timers) clearTimeout(id);
    timers.clear();
  }

  function reset() {
    clearTimers();
    sequence = [];
    inputIdx = 0;
    state = 'idle';
  }

  function flash(i) {
    const pad = pads[i];
    pad.classList.add('simon-pad--active');
    later(() => pad.classList.remove('simon-pad--active'), ON_MS);
  }

  function nextRound() {
    sequence.push(Math.floor(Math.random() * 4));
    showSequence();
  }

  function showSequence() {
    state = 'showing';
    render();
    let delay = 500;
    for (const pad of sequence) {
      const p = pad;
      later(() => flash(p), delay);
      delay += ON_MS + GAP_MS;
    }
    later(() => { state = 'input'; inputIdx = 0; render(); }, delay);
  }

  function press(i) {
    if (state !== 'input') return;
    flash(i);
    if (i === sequence[inputIdx]) {
      inputIdx++;
      if (inputIdx === sequence.length) {
        state = 'showing';
        render();
        later(nextRound, 650);
      }
    } else {
      gameOver();
    }
  }

  function gameOver() {
    state = 'over';
    const reached = sequence.length;
    if (reached > best) { best = reached; writeBest(BEST_KEY, best); }
    render();
  }

  function start() {
    reset();
    nextRound();
    element.focus();
  }

  // --- DOM ---------------------------------------------------------------

  const roundEl = h('b', {}, '0');
  const bestEl = h('b', {}, String(best));
  const overlayEl = h('div', { className: 'chat-game__overlay' });

  const boardEl = h('div', {
    className: 'chat-game__board chat-game__board--simon',
    style: { gridTemplateColumns: 'repeat(2, 1fr)' },
  });
  const pads = [];
  for (let i = 0; i < 4; i++) {
    const idx = i;
    const pad = h('button', {
      className: 'simon-pad', type: 'button', 'aria-label': t('game.pad', { n: i + 1 }),
      style: { background: COLORS[i] },
      onClick: () => press(idx),
    });
    pads.push(pad);
    boardEl.appendChild(pad);
  }

  const title = h('span', { className: 'chat-game__title' }, [
    h('span', { className: 'chat-game__title-icon', innerHTML: icon('grid-dots') }),
    meta.name,
  ]);
  const restartBtn = h('button', {
    className: 'chat-game__icon-btn', title: t('game.newGame'), 'aria-label': t('game.newGame'),
    innerHTML: icon('rotate-ccw'), onClick: start,
  });
  const closeBtn = h('button', {
    className: 'chat-game__icon-btn', title: t('game.closeGame'), 'aria-label': t('game.closeGame'),
    innerHTML: icon('x'),
    onClick: () => { teardown(); if (opts.onClose) opts.onClose(); },
  });

  const header = h('div', { className: 'chat-game__header' }, [
    title,
    h('div', { className: 'chat-game__scores' }, [
      h('div', { className: 'chat-game__score' }, [h('span', {}, t('game.round')), roundEl]),
      h('div', { className: 'chat-game__score' }, [h('span', {}, t('game.best')), bestEl]),
    ]),
    h('div', { className: 'chat-game__actions' }, [restartBtn, closeBtn]),
  ]);

  const stage = h('div', { className: 'chat-game__stage' }, [boardEl, overlayEl]);
  const hint = h('div', { className: 'chat-game__hint' }, t('game.hint.simon'));
  const element = h('div', { className: 'chat-game', tabindex: '0' }, [header, stage, hint]);

  function render() {
    roundEl.textContent = String(sequence.length);
    bestEl.textContent = String(best);
    boardEl.classList.toggle('chat-game__board--locked', state === 'showing');

    if (state === 'idle' || state === 'over') {
      overlayEl.style.display = '';
      const titleText = state === 'over' ? t('game.gameOverRound', { round: sequence.length }) : meta.name;
      overlayEl.replaceChildren(
        h('div', { className: 'chat-game__overlay-title' }, titleText),
        h('button', { className: 'chat-game__overlay-btn', onClick: start },
          state === 'over' ? t('game.playAgain') : t('game.start')),
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
    clearTimers();
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
