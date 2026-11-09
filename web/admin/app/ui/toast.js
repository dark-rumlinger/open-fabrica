/**
 * Toast notification system.
 *
 * A toast stays while it is being read: hovering or focusing it pauses its
 * timer. It can be closed with its own button, an identical toast that is
 * already showing is refreshed instead of stacked (a loop of failures used to
 * pile up a wall of the same message), and at most MAX_VISIBLE show at once.
 */

import { icon } from './icon.js';
import { t } from '../core/i18n.js';

const MAX_VISIBLE = 3;
// After a hover or focus ends, leave the toast up at least this long.
const RESUME_MS = 2000;

let container = null;
// Live toasts, oldest first: { el, type, message, timer, remaining, started }.
const live = [];

function ensureContainer() {
  if (!container) {
    container = document.createElement('div');
    container.className = 'toast-container';
    // Screen readers should announce toasts without stealing focus.
    container.setAttribute('aria-live', 'polite');
    container.setAttribute('aria-atomic', 'false');
    document.body.appendChild(container);
  }
  return container;
}

function schedule(entry, ms) {
  clearTimeout(entry.timer);
  entry.remaining = ms;
  entry.started = Date.now();
  entry.timer = setTimeout(() => dismiss(entry), ms);
}

function pause(entry) {
  clearTimeout(entry.timer);
  entry.remaining = Math.max(entry.remaining - (Date.now() - entry.started), RESUME_MS);
}

function resume(entry) {
  schedule(entry, entry.remaining);
}

/**
 * Show a toast notification.
 * @param {string} message - The message to display.
 * @param {'success'|'error'|'warning'|'info'} [type='info'] - Toast type.
 * @param {number} [duration=4000] - Duration in ms before auto-dismiss.
 * @param {Function} [onClick] - Optional action callback (navigates instead of just dismissing).
 */
export function show(message, type = 'info', duration = 4000, onClick = null) {
  const c = ensureContainer();

  // The same message is already on screen: keep that one and restart its clock.
  // Actions are left alone, since two toasts may lead to different places.
  if (!onClick) {
    const same = live.find((t) => t.type === type && t.message === message && !t.onClick);
    if (same) {
      schedule(same, duration);
      same.el.classList.remove('toast--refresh');
      void same.el.offsetWidth; // restart the nudge animation
      same.el.classList.add('toast--refresh');
      return;
    }
  }

  // Too many at once: the oldest has had its say.
  while (live.length >= MAX_VISIBLE) dismiss(live[0]);

  const el = document.createElement('div');
  el.className = `toast toast--${type}`;
  el.setAttribute('role', type === 'error' ? 'alert' : 'status');

  const text = document.createElement('span');
  text.className = 'toast__message';
  text.textContent = message;
  el.appendChild(text);

  const entry = { el, type, message, onClick, timer: null, remaining: duration, started: Date.now() };

  if (onClick) {
    // The message becomes a real control: reachable by Tab, Enter or Space runs it.
    el.classList.add('toast--clickable');
    text.setAttribute('role', 'button');
    text.setAttribute('tabindex', '0');
    const run = () => { onClick(); dismiss(entry); };
    text.addEventListener('click', run);
    text.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); run(); }
    });
  } else {
    // Click anywhere on a plain toast to dismiss it, as before.
    text.addEventListener('click', () => dismiss(entry));
  }

  const close = document.createElement('button');
  close.type = 'button';
  close.className = 'toast__close';
  close.setAttribute('aria-label', t('toast.dismiss'));
  close.innerHTML = icon('x');
  close.addEventListener('click', () => dismiss(entry));
  el.appendChild(close);

  el.addEventListener('mouseenter', () => pause(entry));
  el.addEventListener('mouseleave', () => resume(entry));
  el.addEventListener('focusin', () => pause(entry));
  el.addEventListener('focusout', () => resume(entry));

  c.appendChild(el);
  live.push(entry);

  // Trigger animation
  requestAnimationFrame(() => {
    el.classList.add('visible');
  });

  schedule(entry, duration);
}

function dismiss(entry) {
  const i = live.indexOf(entry);
  if (i === -1) return;
  live.splice(i, 1);
  clearTimeout(entry.timer);
  const { el } = entry;
  el.classList.remove('visible');
  el.addEventListener('transitionend', () => {
    if (el.parentNode) el.parentNode.removeChild(el);
  });
  // Fallback: remove if transitionend never fires
  setTimeout(() => {
    if (el.parentNode) el.parentNode.removeChild(el);
  }, 500);
}

export function success(msg) { show(msg, 'success'); }
// Errors are the ones people need time to read and copy from.
export function error(msg) { show(msg, 'error', 10000); }
export function warning(msg) { show(msg, 'warning', 6000); }
export function info(msg) { show(msg, 'info'); }
