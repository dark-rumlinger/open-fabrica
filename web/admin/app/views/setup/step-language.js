/**
 * Setup Step 1: choose the interface language.
 *
 * Picking a language saves it and reloads the page, because every string is
 * built from the catalog at load. The setup wizard asks to resume on this same
 * step afterwards (see takeResume), so the person sees the screen re-render in
 * their language and then presses Continue.
 */

import { h } from '../../core/dom.js';
import { SUPPORTED, getLocale, setLocale, t } from '../../core/i18n.js';
import { icon } from '../../ui/icon.js';

const RESUME_KEY = 'of_setup_resume';

// The prompt in each language, cycled in the hero like a new phone's first
// screen, and the second line of each row. These are the languages' own words
// rather than interface copy, so they stay the same whatever language is active.
const PROMPTS = [
  'Choose your language', 'Elige tu idioma', 'Choisissez votre langue', 'Wähle deine Sprache',
  'Escolha seu idioma', 'Оберіть свою мову', 'Выберите язык', '选择你的语言', '言語を選択してください',
  'अपनी भाषा चुनें', 'اختر لغتك',
];
const CONTINUE_IN = {
  en: 'Continue in English',
  es: 'Continuar en español',
  fr: 'Continuer en français',
  de: 'Weiter auf Deutsch',
  pt: 'Continuar em português',
  uk: 'Продовжити українською',
  ru: 'Продолжить на русском',
  zh: '继续使用中文',
  ja: '日本語で続ける',
  hi: 'हिन्दी में जारी रखें',
  ar: 'المتابعة بالعربية',
};
const GREETING_MS = 2200;

/** True once, right after the language step reloaded the page. */
export function takeResume() {
  try {
    const was = sessionStorage.getItem(RESUME_KEY) === '1';
    sessionStorage.removeItem(RESUME_KEY);
    return was;
  } catch { return false; }
}

function markResume() {
  try { sessionStorage.setItem(RESUME_KEY, '1'); } catch { /* storage off: setup restarts at step 1 */ }
}

function greetingHero() {
  const word = h('span', { className: 'lang-hero__word' }, PROMPTS[0]);
  const hero = h('div', { className: 'lang-hero', 'aria-hidden': 'true' }, [word]);

  const reduce = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  if (!reduce) {
    let i = 0;
    const timer = setInterval(() => {
      // The step was removed from the page: stop ticking.
      if (!hero.isConnected) { clearInterval(timer); return; }
      word.classList.add('is-out');
      setTimeout(() => {
        i = (i + 1) % PROMPTS.length;
        word.textContent = PROMPTS[i];
        word.classList.remove('is-out');
      }, 260);
    }, GREETING_MS);
  }
  return hero;
}

export function renderLanguage({ body, setHeader, setActions, next }) {
  // No title: the prompt and the language names say what this screen is.
  setHeader('');

  const current = getLocale();
  const rows = SUPPORTED.map((l) => {
    const selected = l.code === current;
    return h('button', {
      type: 'button',
      className: `lang-option${selected ? ' is-selected' : ''}`,
      role: 'radio',
      'aria-checked': String(selected),
      tabindex: selected ? '0' : '-1',
      lang: l.tag,
      'data-code': l.code,
      onClick: () => choose(l.code),
    }, [
      h('span', { className: 'lang-option__text' }, [
        h('span', { className: 'lang-option__name' }, l.label),
        h('span', { className: 'lang-option__sub' }, CONTINUE_IN[l.code]),
      ]),
      h('span', { className: 'lang-option__check', innerHTML: icon('check') }),
    ]);
  });

  function choose(code) {
    if (code === current) return;
    if (setLocale(code)) {
      markResume();
      window.location.reload();
    }
  }

  // Arrow keys move through the group, like a native radio set.
  const list = h('div', {
    className: 'lang-list',
    role: 'radiogroup',
    'aria-label': t('language.label'),
    onkeydown: (e) => {
      const keys = { ArrowDown: 1, ArrowRight: 1, ArrowUp: -1, ArrowLeft: -1 };
      if (!(e.key in keys)) return;
      e.preventDefault();
      const at = rows.findIndex((r) => r === document.activeElement);
      const to = rows[(Math.max(at, 0) + keys[e.key] + rows.length) % rows.length];
      to.focus();
    },
  }, rows);

  body.appendChild(greetingHero());
  body.appendChild(list);

  setActions([
    h('button', { className: 'btn btn--primary', onClick: next }, t('common.continue')),
  ]);

  const selectedRow = rows.find((r) => r.classList.contains('is-selected'));
  if (selectedRow) selectedRow.focus();
}
