/**
 * Reusable brand splash shared by the setup wizard and the login screen: a
 * logo moment that FLIP-morphs into the brand mark in the auth bar (pass it as
 * `target`). { weave: true } prepends the loom-weave sequence (reserved for
 * first-run setup). The target must already be mounted and the card hidden
 * via its preboot class, so we can measure the mark's real position and land
 * the morph exactly on top of it.
 */

import { h } from '../core/dom.js';
import { t } from '../core/i18n.js';

// The brand tagline, exported so the login page, setup wizard, and the About dialog's
// AFK screen all quote the same line.
export const TAGLINE = t('brand.tagline');
const HOLD_MS = 2600;       // default time before auto-advancing into the card
const TAGLINE_DELAY_MS = 700;
const MORPH_FALLBACK_MS = 900;

// Loom-weave timeline (ms from entrance). The cloth draws, catches the light,
// then resolves into the mark; the mark lingers a beat before morphing.
const WEAVE = {
  weftAt: 560,
  sheenAt: 1280,
  resolveAt: 2150,
  taglineAfter: 420,
  advanceAfter: 1450,
};

const SVGNS = 'http://www.w3.org/2000/svg';
function svgEl(tag, attrs) {
  const n = document.createElementNS(SVGNS, tag);
  if (attrs) for (const k in attrs) n.setAttribute(k, String(attrs[k]));
  return n;
}

// Build the woven grid: N warp lines (drawn top-to-bottom) and N weft lines
// (shuttled from alternating sides). Every third thread carries the accent so
// the cloth reads as fabric rather than a plain grid. Threads start hidden
// (CSS dash) and reveal when given .is-drawn.
function buildWeave() {
  const N = 9;
  const pad = 12;
  const span = 100 - pad * 2;
  const step = span / (N - 1);
  const svg = svgEl('svg', {
    class: 'splash__weave', viewBox: '0 0 100 100', 'aria-hidden': 'true',
  });
  const warp = [];
  const weft = [];
  for (let i = 0; i < N; i++) {
    const x = pad + i * step;
    const ln = svgEl('line', {
      class: 'splash__thread' + (i % 3 === 0 ? ' splash__thread--accent' : ''),
      x1: x, y1: 8, x2: x, y2: 92, pathLength: 1,
    });
    ln.style.transition = `stroke-dashoffset 0.5s ease ${(i * 0.045).toFixed(3)}s`;
    warp.push(ln);
    svg.appendChild(ln);
  }
  for (let j = 0; j < N; j++) {
    const y = pad + j * step;
    const rev = j % 2 === 1; // alternate shuttle direction for a woven feel
    const ln = svgEl('line', {
      class: 'splash__thread' + (j % 3 === 1 ? ' splash__thread--accent' : ''),
      x1: rev ? 92 : 8, y1: y, x2: rev ? 8 : 92, y2: y, pathLength: 1,
    });
    ln.style.transition = `stroke-dashoffset 0.5s ease ${(j * 0.045).toFixed(3)}s`;
    weft.push(ln);
    svg.appendChild(ln);
  }
  return { svg, warp, weft };
}

export function runSplash(wrapper, card, opts = {}) {
  const {
    // Where the logo lands. Falls back to a logo inside the card.
    target: targetEl = null,
    logoSelector = '.brand-mark',
    inputSelector = '.input',
    prebootClass = 'auth-card--preboot',
    revealClass = 'auth-card--revealing',
    // Loom-weave intro. First-run setup opts in; login stays a simple reveal.
    weave = false,
    // How long the brand moment lingers before morphing into the card. The
    // tagline reveal scales with it so it still lands before the morph.
    holdMs = HOLD_MS,
    taglineDelayMs = Math.round(TAGLINE_DELAY_MS * (holdMs / HOLD_MS)),
  } = opts;

  const reduce = window.matchMedia
    && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  const useWeave = weave && !reduce;

  const glow = h('div', { className: 'splash__glow' });
  const logo = h('img', {
    className: 'splash__logo',
    src: '/openfabrica_logo.svg',
    alt: 'Open Fabrica',
  });
  // Stage stacks the (optional) cloth, its sheen, and the logo in one centered
  // cell so the mark resolves exactly where the weave was.
  const stage = h('div', { className: 'splash__stage' }, [logo]);
  const tagline = h('p', { className: 'splash__tagline' }, TAGLINE);
  const overlay = h('div', { className: 'splash' }, [glow, stage, tagline]);

  let loom = null;
  if (useWeave) {
    loom = buildWeave();
    loom.sheen = h('div', { className: 'splash__sheen' });
    stage.insertBefore(loom.svg, logo);
    stage.insertBefore(loom.sheen, logo);
    overlay.classList.add('has-weave');
  }
  wrapper.appendChild(overlay);

  let advanced = false;
  let resolved = !useWeave;
  let autoTimer = null;
  const timers = [];

  function bind() {
    document.addEventListener('keydown', onInput, true);
    overlay.addEventListener('click', onInput);
    overlay.addEventListener('wheel', onInput, { passive: true });
    overlay.addEventListener('touchstart', onInput, { passive: true });
  }
  function unbind() {
    document.removeEventListener('keydown', onInput, true);
    overlay.removeEventListener('click', onInput);
    overlay.removeEventListener('wheel', onInput);
    overlay.removeEventListener('touchstart', onInput);
  }
  function onInput() { advance(); }

  function focusInput() {
    const input = card.querySelector(inputSelector);
    if (input) { try { input.focus(); } catch (_) { /* ignore */ } }
  }

  function revealCard() {
    card.classList.remove(prebootClass);
    card.classList.add(revealClass);
  }

  // Fade the cloth out and let the logo bloom in its place. Idempotent so a
  // skip mid-weave lands the mark before the morph runs.
  function resolveWeave() {
    if (resolved) return;
    resolved = true;
    overlay.classList.add('is-in', 'is-weave-resolved');
  }

  function advance() {
    if (advanced) return;
    advanced = true;
    if (autoTimer) clearTimeout(autoTimer);
    timers.forEach(clearTimeout);
    unbind();

    if (reduce) {
      overlay.classList.add('is-hiding');
      revealCard();
      setTimeout(() => { overlay.remove(); focusInput(); }, 320);
      return;
    }

    // Ensure the mark is present (no-op unless a weave was skipped early).
    resolveWeave();

    // FLIP: measure where the logo is now and where the target mark sits,
    // then translate + scale the splash logo onto that target.
    const target = targetEl || card.querySelector(logoSelector);
    const from = logo.getBoundingClientRect();
    const to = target ? target.getBoundingClientRect() : from;
    const dx = (to.left + to.width / 2) - (from.left + from.width / 2);
    const dy = (to.top + to.height / 2) - (from.top + from.height / 2);
    const scale = from.width ? to.width / from.width : 1;

    // The landing spot is visible once the backdrop dissolves; hide it while
    // the splash logo is in flight so the mark arrives rather than doubles.
    if (target) target.style.visibility = 'hidden';

    overlay.classList.add('is-morphing');
    requestAnimationFrame(() => {
      logo.style.transform = `translate(${dx}px, ${dy}px) scale(${scale})`;
    });
    // Let the card start blooming a beat after the logo launches so it feels
    // like the mark is pulling the card into existence.
    setTimeout(revealCard, 180);

    let finished = false;
    function finish() {
      if (finished) return;
      finished = true;
      if (target) target.style.visibility = '';
      overlay.remove();
      focusInput();
    }
    logo.addEventListener('transitionend', (e) => {
      if (e.propertyName === 'transform') finish();
    });
    setTimeout(finish, MORPH_FALLBACK_MS); // safety net if transitionend is missed
  }

  // Entrance.
  requestAnimationFrame(() => {
    if (useWeave) {
      overlay.classList.add('is-weaving');
      loom.warp.forEach((l) => l.classList.add('is-drawn'));
    } else {
      overlay.classList.add('is-in');
      setTimeout(() => overlay.classList.add('show-tagline'), taglineDelayMs);
    }
  });

  if (useWeave) {
    timers.push(setTimeout(() => loom.weft.forEach((l) => l.classList.add('is-drawn')), WEAVE.weftAt));
    timers.push(setTimeout(() => loom.sheen.classList.add('is-sweeping'), WEAVE.sheenAt));
    timers.push(setTimeout(resolveWeave, WEAVE.resolveAt));
    timers.push(setTimeout(() => overlay.classList.add('show-tagline'), WEAVE.resolveAt + WEAVE.taglineAfter));
  }

  const advanceAt = useWeave
    ? WEAVE.resolveAt + WEAVE.advanceAfter
    : (reduce ? Math.min(holdMs, 1500) : holdMs);
  autoTimer = setTimeout(advance, advanceAt);
  bind();
}
