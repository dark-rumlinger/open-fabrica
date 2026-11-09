/**
 * The About dialog: a deliberately out-of-place 90s/2000s "keygen" panel, with
 * its starfield, chiptune, and BSOD gag. Lifted out of the sidebar unchanged
 * when the sidebar was replaced by the top bar.
 */

import { h } from '../core/dom.js';
import { t } from '../core/i18n.js';
import * as state from '../core/state.js';
import * as modal from './modal.js';
import { createAboutMusic } from './about-music.js';
import { TAGLINE } from './splash.js';
// Whether the About-dialog "patch" easter egg has ever been triggered, kept in
// localStorage so a later visit to the About screen shows the "registered"
// reward state instead of the pre-patch warning.
const PATCH_KEY = 'openfabrica:patched';

function isPatched() {
  try { return localStorage.getItem(PATCH_KEY) === '1'; } catch { return false; }
}

function markPatched() {
  try { localStorage.setItem(PATCH_KEY, '1'); } catch { /* storage off: gag still runs, just not remembered */ }
}

/** A pseudo-random XXXX-XXXX-XXXX-XXXX serial for the "registered" reward. */
function fakeSerial() {
  const hex = '0123456789ABCDEF';
  const group = () => Array.from({ length: 4 }, () => hex[Math.floor(Math.random() * 16)]).join('');
  return `${group()}-${group()}-${group()}-${group()}`;
}

/**
 * Warping starfield backdrop for the About "keygen" dialog.
 * Draws to the given canvas until the returned stop() is called (on modal
 * close), then releases the animation frame. Guarded so a missing 2D context
 * (or a zero-size canvas) is a silent no-op rather than an About-dialog crash.
 * @param {HTMLCanvasElement} canvas
 * @returns {Function} stop - halts the loop.
 */
function startStarfield(canvas) {
  const ctx = canvas.getContext && canvas.getContext('2d');
  if (!ctx) return () => {};
  // The warp is constant motion; the global reduced-motion rule only reaches
  // CSS, so honour the preference here too and leave the canvas still.
  if (window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches) return () => {};
  let raf = 0;
  let stars = [];
  let w = 0;
  let h2 = 0;

  function size() {
    const r = canvas.getBoundingClientRect();
    const dpr = window.devicePixelRatio || 1;
    w = canvas.width = Math.max(1, r.width * dpr);
    h2 = canvas.height = Math.max(1, r.height * dpr);
    stars = Array.from({ length: 200 }, () => ({
      x: Math.random() * w, y: Math.random() * h2, z: Math.random() * w,
    }));
  }
  size();
  window.addEventListener('resize', size);

  function frame() {
    ctx.fillStyle = '#0a0a30';
    ctx.fillRect(0, 0, w, h2);
    for (const s of stars) {
      s.z -= 3.5;
      if (s.z <= 0) { s.z = w; s.x = Math.random() * w; s.y = Math.random() * h2; }
      const k = w / s.z;
      const x = (s.x - w / 2) * k + w / 2;
      const y = (s.y - h2 / 2) * k + h2 / 2;
      if (x < 0 || x > w || y < 0 || y > h2) continue;
      const b = 1 - s.z / w;
      ctx.fillStyle = `rgba(180,200,255,${b})`;
      ctx.fillRect(x, y, b * 2.2, b * 2.2);
    }
    raf = requestAnimationFrame(frame);
  }
  raf = requestAnimationFrame(frame);

  return () => {
    if (raf) cancelAnimationFrame(raf);
    window.removeEventListener('resize', size);
  };
}

/**
 * Play a tongue-in-cheek full-screen "Blue Screen of Death" - the payoff for
 * clicking APPLY PATCH, styled like a Windows NT/2000 stop error. Runs a fake
 * memory-dump counter to 100%, then waits for any key/click (or auto-reboots
 * after a few seconds) and calls onReboot. While it's up it swallows all keys
 * so nothing leaks to the modal underneath.
 * @param {Function} onReboot - invoked once, when the screen is dismissed.
 */
function showBsod(onReboot) {
  const stop = '*** STOP: 0x0000C0DE (0x1337BEEF, 0xDEADBEEF, 0x00000000, 0xFABR1CA5)';
  const intro =
    'A problem has been detected and Windows has been shut down to prevent ' +
    'damage to your computer.\n\n' +
    'UNREGISTERED_SOFTWARE_DETECTED\n\n' +
    'The problem seems to be caused by the following file: OPENFABRICA.SYS\n\n' +
    'If this is the first time you\'ve seen this stop error screen, restart ' +
    'your computer. If this screen appears again, follow these steps:\n\n' +
    'Check to be sure you have adequate disk space. Disable BIOS memory ' +
    'options such as caching or shadowing.\n\n' +
    'Technical information:\n\n' +
    stop + '\n\n' +
    '***  OPENFABRICA.SYS - Address 0xFABR1CA5 base at 0xF00DFACE\n\n';

  const pct = h('span', {}, '0');
  const dump = h('div', {}, ['Dumping physical memory to disk: ', pct]);
  const caret = h('span', { className: 'bsod__caret' }, '_');
  const prompt = h('div', { className: 'bsod__prompt' }, ['Press any key to continue ', caret]);
  prompt.style.visibility = 'hidden';

  const screen = h('div', {
    className: 'bsod', role: 'alertdialog', 'aria-label': t('about.systemError'), tabindex: '-1',
  }, [
    h('div', { className: 'bsod__text' }, [intro, dump, prompt]),
  ]);
  document.body.appendChild(screen);
  screen.focus();

  let done = false;
  let rebootTimer = null;
  let p = 0;
  const fill = setInterval(() => {
    p = Math.min(100, p + Math.floor(Math.random() * 6) + 3);
    pct.textContent = String(p);
    if (p >= 100) {
      clearInterval(fill);
      done = true;
      prompt.style.visibility = 'visible';
      rebootTimer = setTimeout(dismiss, 3000); // auto-reboot if they just stare
    }
  }, 120);

  function dismiss() {
    clearInterval(fill);
    if (rebootTimer) clearTimeout(rebootTimer);
    document.removeEventListener('keydown', onKey, true);
    screen.removeEventListener('click', onClick);
    if (screen.parentNode) screen.parentNode.removeChild(screen);
    onReboot();
  }
  // Swallow every key while the BSOD is up (so ESC etc. don't reach the modal);
  // once the dump finishes, any key dismisses.
  function onKey(e) { e.preventDefault(); e.stopPropagation(); if (done) dismiss(); }
  function onClick() { if (done) dismiss(); }
  document.addEventListener('keydown', onKey, true);
  screen.addEventListener('click', onClick);
}

/** A themed external link for the About "keygen" dialog. */
function aboutLink(href, label) {
  return h('a', {
    href, target: '_blank', rel: 'noopener noreferrer',
    className: 'afk__link', title: label,
  }, label);
}

/**
 * Open the About dialog: a deliberately out-of-place 90s/2000s "keygen" panel -
 * Amiga/warez-BBS blue, animated copper bars, starfield, and a chiptune score.
 * Keeps the version, author, community links, and the mental-health note.
 * Closable the usual three ways: the header X, ESC, or the Close button.
 */
export function showAboutModal() {
  const sysStatus = state.get('systemStatus') || {};
  const version = sysStatus.version ? `v${sysStatus.version}` : t('about.versionUnknown');

  const stars = h('canvas', { className: 'afk__stars', 'aria-hidden': 'true' });

  // Square portrait slot, wrapped in an animated copper ring. Falls back to a
  // labelled placeholder if the image isn't present yet, so the panel never
  // shows a broken-image icon.
  const portrait = h('img', {
    className: 'afk__img',
    src: '/dark.jpg',
    alt: 'Dark Rumlinger',
    onLoad: () => { photo.classList.remove('afk__photo--loading'); photo.classList.add('afk__photo--ready'); },
    onError: (e) => { e.currentTarget.remove(); photo.classList.remove('afk__photo--loading'); photo.classList.add('afk__photo--empty'); },
  });
  // Starts in the "loading" state (a decoding scanline) so the slot never
  // flashes empty before the image arrives; the load/error handlers swap it to
  // ready or the placeholder.
  const photo = h('div', { className: 'afk__photo afk__photo--loading' }, [portrait]);
  // If the image was already cached, its load/error may have fired before the
  // listeners attached - reflect the final state right away.
  if (portrait.complete) {
    if (portrait.naturalWidth > 0) { photo.classList.remove('afk__photo--loading'); photo.classList.add('afk__photo--ready'); }
    else { portrait.remove(); photo.classList.remove('afk__photo--loading'); photo.classList.add('afk__photo--empty'); }
  }
  const face = h('div', { className: 'afk__face' }, [
    h('div', { className: 'afk__frame' }),
    photo,
  ]);

  // Once the patch has been "applied" (the BSOD gag has run at least once), the
  // About screen switches to its reward state: a generated serial appears in
  // the info rows and the amber warning becomes a green "registered" banner.
  const patched = isPatched();

  const panel = h('div', { className: 'afk' }, [
    h('div', { className: 'afk__titlebar' }, '· O P E N   F A B R I C A ·'),
    h('div', { className: 'afk__copper' }),
    h('div', { className: 'afk__body' }, [
      face,
      h('div', { className: 'afk__col' }, [
        h('div', { className: 'afk__bigname' }, 'Open Fabrica'),
        h('div', { className: 'afk__slogan' }, TAGLINE),
        h('div', { className: 'afk__rows' }, [
          h('div', {}, [h('span', { className: 'afk__lbl' }, t('about.version')), h('b', {}, version)]),
          h('div', {}, [h('span', { className: 'afk__lbl' }, t('about.author')), h('b', {}, 'Dark Rumlinger')]),
          patched
            ? h('div', {}, [h('span', { className: 'afk__lbl' }, t('about.serial')), h('b', { className: 'afk__serial' }, fakeSerial())])
            : null,
          h('div', { className: 'afk__contact' }, [
            h('span', { className: 'afk__lbl' }, t('about.links')),
            aboutLink('https://github.com/dark-rumlinger/open-fabrica', 'GitHub'),
            aboutLink('https://x.com/open_fabrica', 'X'),
          ]),
        ]),
        h('div', { className: 'afk__note' }, t('about.note')),
      ]),
    ]),
    patched
      ? h('div', { className: 'afk__warn afk__warn--done' }, t('about.patched'))
      : h('div', { className: 'afk__warn' }, t('about.warning')),
    h('div', { className: 'afk__copper' }),
  ]);

  const body = h('div', { className: 'about-keygen' }, [stars, panel]);

  // Generated chiptune, started within this click (a user gesture, so autoplay
  // is allowed). Both the score and the starfield are torn down via the modal's
  // onClose so they stop on any close path - the X, ESC, or the Close button.
  const music = createAboutMusic();
  music.start();
  const stopStars = startStarfield(stars);

  // Empty title: the keygen panel is the whole story, so the modal header
  // collapses to just the (themed) footer button. Before the patch is applied
  // that button is a fake ".exe patcher" action: clicking it fires the BSOD gag,
  // then reboots back to the app by closing the dialog (returning false keeps
  // the modal open behind the BSOD until then). Once registered, the gag is
  // spent, so it's just a plain Close. The header X and ESC always close
  // normally, gag-free.
  const patchAction = patched
    ? { label: t('common.close'), className: 'btn', onClick: () => {} }
    : { label: t('about.applyPatch'), className: 'btn', onClick: () => { markPatched(); showBsod(() => modal.close()); return false; } };
  modal.show('', body, [patchAction], { ariaLabel: t('nav.about'), onClose: () => { music.stop(); stopStars(); } });
}
