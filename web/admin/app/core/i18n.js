/**
 * Interface translations.
 *
 * The language is the one the person saved (localStorage), else the first of the
 * browser's preferred languages that Open Fabrica is translated into, else
 * English. A detected language is not saved, so it keeps following the browser
 * until the person picks one. The catalog for the chosen language is loaded with
 * a top-level await, so by the time any importing module runs, `t()` already
 * answers from the right catalog and module-level constants may call it. English
 * is the fallback for any key a catalog does not carry.
 *
 * Changing the language persists the choice only; the caller reloads the page
 * so every string is rebuilt from the new catalog.
 */

export const LOCALE_KEY = 'of_locale';
export const DEFAULT_LOCALE = 'en';

/**
 * Each entry: code (stored), label (shown in its own language), tag (BCP 47, for
 * Intl), and `dir: 'rtl'` for right-to-left languages (left-to-right otherwise).
 */
export const SUPPORTED = [
  { code: 'en', label: 'English', tag: 'en' },
  { code: 'es', label: 'Español', tag: 'es' },
  { code: 'fr', label: 'Français', tag: 'fr' },
  { code: 'de', label: 'Deutsch', tag: 'de' },
  { code: 'pt', label: 'Português', tag: 'pt-BR' },
  { code: 'uk', label: 'Українська', tag: 'uk' },
  { code: 'ru', label: 'Русский', tag: 'ru' },
  { code: 'zh', label: '中文', tag: 'zh-CN' },
  { code: 'ja', label: '日本語', tag: 'ja' },
  { code: 'hi', label: 'हिन्दी', tag: 'hi-IN' },
  { code: 'ar', label: 'العربية', tag: 'ar', dir: 'rtl' },
];

const LOADERS = {
  en: () => import('../locales/en.js'),
  es: () => import('../locales/es.js'),
  fr: () => import('../locales/fr.js'),
  de: () => import('../locales/de.js'),
  pt: () => import('../locales/pt.js'),
  uk: () => import('../locales/uk.js'),
  ru: () => import('../locales/ru.js'),
  zh: () => import('../locales/zh.js'),
  ja: () => import('../locales/ja.js'),
  hi: () => import('../locales/hi.js'),
  ar: () => import('../locales/ar.js'),
};

function readSaved() {
  try {
    return localStorage.getItem(LOCALE_KEY);
  } catch { return null; /* storage off: use the default */ }
}

function isSupported(code) {
  return SUPPORTED.some((l) => l.code === code);
}

/**
 * The supported language a browser's preference list asks for, or English.
 * Preferences are tried in order, so ['it', 'de'] gives German, the closest thing
 * to what the person asked for that we have. Only the language part counts
 * ('pt-PT' is Portuguese), with one exception: Chinese is Simplified, so Traditional
 * Chinese (zh-TW, zh-HK, zh-MO, zh-Hant) is not treated as served.
 * @param {readonly string[]} preferred BCP 47 tags, most preferred first
 * @returns {string} one of SUPPORTED's codes
 */
export function matchLocale(preferred) {
  for (const raw of preferred || []) {
    if (typeof raw !== 'string') continue;
    const parts = raw.trim().toLowerCase().replace(/_/g, '-').split('-');
    const code = parts[0];
    if (!isSupported(code)) continue;
    if (code === 'zh' && parts.slice(1).some((p) => ['tw', 'hk', 'mo', 'hant'].includes(p))) continue;
    return code;
  }
  return DEFAULT_LOCALE;
}

function browserLanguages() {
  // Only a page in a browser has a browser language. Under Node (the tests) the
  // machine's locale would otherwise leak in and change which catalog loads.
  if (typeof document === 'undefined') return [];
  try {
    const nav = globalThis.navigator;
    if (!nav) return [];
    return nav.languages && nav.languages.length ? nav.languages : [nav.language];
  } catch { return []; }
}

const saved = readSaved();
const locale = isSupported(saved) ? saved : matchLocale(browserLanguages());
const entry = SUPPORTED.find((l) => l.code === locale);
const tag = entry.tag;
const dir = entry.dir === 'rtl' ? 'rtl' : 'ltr';

const english = (await LOADERS.en()).default;
const active = locale === 'en' ? english : (await LOADERS[locale]()).default;

const activeRules = new Intl.PluralRules(tag);
const englishRules = new Intl.PluralRules('en');

if (typeof document !== 'undefined' && document.documentElement) {
  document.documentElement.lang = tag;
  document.documentElement.dir = dir;
}

/** The active language code, always one of SUPPORTED. */
export function getLocale() {
  return locale;
}

/** 'rtl' for right-to-left languages, 'ltr' otherwise. */
export function getDir() {
  return dir;
}

/** BCP 47 tag for Intl and Date formatting, e.g. 'zh-CN'. */
export function intlTag() {
  return tag;
}

/**
 * Save the language. Takes effect on the next page load.
 * @param {string} code one of SUPPORTED
 * @returns {boolean} false when the code is unknown or storage is unavailable
 */
export function setLocale(code) {
  if (!isSupported(code)) return false;
  try {
    localStorage.setItem(LOCALE_KEY, code);
    return true;
  } catch { return false; }
}

function fill(text, params) {
  if (!params) return text;
  return text.replace(/\{(\w+)\}/g, (whole, name) => (name in params ? String(params[name]) : whole));
}

/** True when the active catalog or the English fallback defines this key. */
export function hasKey(key) {
  return active[key] !== undefined || english[key] !== undefined;
}

/**
 * Translate a key, filling {name} placeholders from params. A key missing from
 * the active catalog falls back to English, then to the key itself.
 * @param {string} key
 * @param {Record<string, string|number>} [params]
 * @returns {string}
 */
export function t(key, params) {
  const value = active[key] !== undefined ? active[key] : english[key];
  return typeof value === 'string' ? fill(value, params) : key;
}

/**
 * Translate a counted phrase. The catalog value is an object keyed by plural
 * category (zero/one/two/few/many/other); `{count}` is always available.
 * @param {string} key
 * @param {number} count
 * @param {Record<string, string|number>} [params]
 * @returns {string}
 */
export function tn(key, count, params) {
  const inActive = active[key] !== undefined;
  const forms = inActive ? active[key] : english[key];
  if (!forms || typeof forms !== 'object') return key;
  const category = (inActive ? activeRules : englishRules).select(count);
  const text = forms[category] !== undefined ? forms[category] : forms.other;
  return typeof text === 'string' ? fill(text, { count, ...params }) : key;
}
