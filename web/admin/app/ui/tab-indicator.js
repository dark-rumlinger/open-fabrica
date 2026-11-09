/**
 * Browser-tab indicator for pending LLM questions.
 *
 * Pulses a red dot on the favicon and prefixes the document title with the
 * pending count, so a user on another tab notices the agent is waiting on
 * them. The pulse pauses while the tab is visible (the in-app toast/badge
 * already covers that case).
 */

const BASE_TITLE = 'Open Fabrica';
// The current page's own part of the title ("Users", "Pages | Bakery"). Kept
// here because the pending-question count prefixes the same string.
let pageTitle = '';
// Render the tab icon from the dedicated favicon (solid black disc + solid
// white F), not the in-page logo (a transparent cut-out F). The logo's cut-out
// would let the tab background show through the F on the canvas render.
const FAVICON_HREF = '/openfabrica_favicon.svg';
const ICON_SIZE = 32;
const PULSE_MS = 700;

let iconLink = null;
let idleHref = null;
let alertHref = null;
let ready = false;
let pending = 0;
let timer = null;
let phase = false;

function getIconLink() {
  if (iconLink) return iconLink;
  iconLink = document.querySelector('link[rel="icon"]');
  if (!iconLink) {
    iconLink = document.createElement('link');
    iconLink.rel = 'icon';
    document.head.appendChild(iconLink);
  }
  return iconLink;
}

function drawIcon(img, withDot) {
  const canvas = document.createElement('canvas');
  canvas.width = ICON_SIZE;
  canvas.height = ICON_SIZE;
  const ctx = canvas.getContext('2d');
  ctx.drawImage(img, 0, 0, ICON_SIZE, ICON_SIZE);
  if (withDot) {
    const r = ICON_SIZE * 0.28;
    const cx = ICON_SIZE - r - 1;
    const cy = r + 1;
    ctx.beginPath();
    ctx.arc(cx, cy, r, 0, Math.PI * 2);
    ctx.fillStyle = '#e23b3b';
    ctx.fill();
    ctx.lineWidth = 1.5;
    ctx.strokeStyle = '#ffffff';
    ctx.stroke();
  }
  return canvas.toDataURL('image/png');
}

function preloadIcons() {
  return new Promise(resolve => {
    const img = new Image();
    img.onload = () => {
      try {
        idleHref = drawIcon(img, false);
        alertHref = drawIcon(img, true);
        ready = true;
      } catch {
        // Tainted canvas (cross-origin SVG): fall back to swapping the raw href.
        idleHref = FAVICON_HREF;
        alertHref = FAVICON_HREF;
        ready = true;
      }
      resolve();
    };
    img.onerror = () => { ready = false; resolve(); };
    img.src = FAVICON_HREF;
  });
}

function setFavicon(href) {
  if (!href) return;
  getIconLink().href = href;
}

function stopPulse() {
  if (timer) {
    clearInterval(timer);
    timer = null;
  }
}

function startPulse() {
  stopPulse();
  if (!ready || pending <= 0) return;
  phase = false;
  setFavicon(alertHref);
  timer = setInterval(() => {
    phase = !phase;
    setFavicon(phase ? idleHref : alertHref);
  }, PULSE_MS);
}

function fullTitle() {
  return pageTitle ? `${pageTitle} | ${BASE_TITLE}` : BASE_TITLE;
}

function render() {
  if (pending <= 0) {
    stopPulse();
    document.title = fullTitle();
    if (ready) setFavicon(idleHref);
    return;
  }

  const label = pending > 9 ? '9+' : String(pending);
  document.title = `(${label}) ${fullTitle()}`;

  if (!ready) return;

  if (document.visibilityState === 'visible') {
    stopPulse();
    setFavicon(alertHref);
  } else {
    startPulse();
  }
}

document.addEventListener('visibilitychange', () => render());

/**
 * Update the pending-question count. Pass 0 to clear the indicator.
 */
export function setPending(count) {
  pending = Math.max(0, count | 0);
  render();
}

/**
 * Set the page's part of the document title. Pass '' to fall back to the app name.
 */
export function setPageTitle(title) {
  pageTitle = title || '';
  render();
}

// Preload icons up-front so the first setPending() call can pulse immediately.
preloadIcons().then(() => { if (pending > 0) render(); });
