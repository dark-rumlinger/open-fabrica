/**
 * A few slow, faint dots rising behind the home page. Decoration only: it takes
 * no pointer events, is hidden from assistive tech, and the stylesheet drops it
 * entirely under prefers-reduced-motion. Positions and speeds are randomised per
 * mount; the animation itself is CSS (transform and opacity only).
 */

import { h } from '../core/dom.js';

const between = (min, max) => min + Math.random() * (max - min);

// Mostly small dots with some medium and a few large soft ones. Bigger dots are
// fainter and slower, which reads as depth without drawing the eye.
const TIERS = [
  { name: 'lg', share: 3, size: [9, 14], seconds: [40, 66] },
  { name: 'md', share: 8, size: [5, 8], seconds: [30, 54] },
  { name: 'sm', share: Infinity, size: [2, 4.5], seconds: [22, 48] },
];

function tierAt(index) {
  let upTo = 0;
  for (const tier of TIERS) {
    upTo += tier.share;
    if (index < upTo) return tier;
  }
  return TIERS[TIERS.length - 1];
}

/**
 * @param {number} [count] - how many dots
 * @returns {HTMLElement}
 */
export function createMotes(count = 26) {
  const layer = h('div', { className: 'home-motes', 'aria-hidden': 'true' });
  for (let i = 0; i < count; i++) {
    const tier = tierAt(i);
    const size = between(...tier.size);
    const mote = h('span', { className: `home-mote home-mote--${tier.name}` });
    mote.style.left = `${between(2, 98).toFixed(1)}%`;
    mote.style.width = `${size.toFixed(1)}px`;
    mote.style.height = `${size.toFixed(1)}px`;
    mote.style.animationDuration = `${between(...tier.seconds).toFixed(1)}s`;
    // A negative delay starts each dot part-way up, so the page is never empty at load.
    mote.style.animationDelay = `-${between(0, tier.seconds[1]).toFixed(1)}s`;
    layer.appendChild(mote);
  }
  return layer;
}
