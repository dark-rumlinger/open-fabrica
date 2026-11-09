/**
 * Small round avatar chips for the chat feed. Gives both the owner and Open
 * Fabrica a consistent visual identity in every message badge.
 */

import { h } from '../../core/dom.js';

/**
 * Extract up to two initials from a name (e.g. "Ada Lovelace" -> "AL").
 */
function initialsOf(name) {
  const parts = name.trim().split(/\s+/).filter(Boolean);
  if (!parts.length) return '?';
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
}

/**
 * Create an avatar chip. kind 'brain' renders the branded Open Fabrica mark;
 * anything else renders theme-flipping monochrome initials from `name`.
 */
export function createAvatar(name, kind = 'user') {
  if (kind === 'brain') {
    // Branded Open Fabrica mark. Single black SVG, inverted to white in dark
    // mode via CSS, same source file the top bar uses.
    return h('img', {
      className: 'chat-avatar chat-avatar--brain',
      src: '/openfabrica_logo.svg',
      alt: 'Open Fabrica',
      'aria-hidden': 'true',
    });
  }

  const safeName = (name || '').trim() || 'You';
  // Two-letter initials. Colors are handled in CSS and flip with the theme:
  // white chip / black letters in dark mode, reversed in light mode - mirroring
  // the brand avatar so both badges share one monochrome identity.
  return h('span', {
    className: 'chat-avatar chat-avatar--user',
    'aria-hidden': 'true',
  }, initialsOf(safeName));
}
