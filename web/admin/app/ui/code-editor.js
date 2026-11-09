/**
 * Dependency-free code editor: a syntax-highlighted <pre> layer behind a
 * transparent <textarea>, with a line-number gutter and a current-line band.
 * Deliberately no external CDN or editor bundle, so the admin panel stays fully
 * functional offline and never phones home.
 *
 * Highlighting is cosmetic: the textarea always holds the real value, so a
 * tokenizer miss degrades to plain text, never to a lost or corrupted edit. The
 * highlight layer only ever renders escaped text wrapped in <span> tags, so page
 * source shown in the editor can never execute.
 */

import { escapeHtml } from '../core/utils.js';

const INDENT = '  ';
// Must match css/code-editor.css: the gutter, highlight layer and textarea share
// these metrics so the three layers line up to the pixel.
const LINE_HEIGHT = 20; // px
const PAD_TOP = 8; // px

// Token rules per language. Each rule's own alternations use (?:...) so the only
// capturing group is the rule itself; tokenize() maps a match back to its class
// by capture-group index. Order matters: comments and strings come first so a
// keyword sitting inside one is not recoloured.
const JS_RULES = [
  ['comment', /\/\/[^\n]*|\/\*[\s\S]*?\*\//],
  ['string', /`(?:\\.|[^`\\])*`|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'/],
  ['number', /\b0x[\da-fA-F]+\b|\b\d[\d_]*(?:\.\d+)?(?:[eE][+-]?\d+)?\b/],
  ['keyword', /\b(?:const|let|var|function|return|if|else|for|while|do|switch|case|break|continue|new|class|extends|super|this|typeof|instanceof|in|of|await|async|yield|import|export|from|as|default|try|catch|finally|throw|delete|void|null|true|false|undefined)\b/],
];
const CSS_RULES = [
  ['comment', /\/\*[\s\S]*?\*\//],
  ['string', /"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'/],
  ['atrule', /@[\w-]+/],
  ['number', /#[\da-fA-F]{3,8}\b|\b\d+(?:\.\d+)?(?:px|em|rem|%|vh|vw|vmin|vmax|s|ms|fr|deg|pt)?\b/],
  ['property', /[\w-]+(?=\s*:)/],
];
const HTML_RULES = [
  ['comment', /<!--[\s\S]*?-->/],
  ['tag', /<\/?[a-zA-Z][\w-]*|\/?>/],
  ['string', /"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'/],
  ['attr', /[a-zA-Z_:][\w:.-]*(?=\s*=)/],
];

// rulesFor maps a filename extension to a rule set, or null for "render as plain
// text" (the correct result for a type with no highlighter, not a fallback shim).
export function rulesFor(filename) {
  const ext = (String(filename).split('.').pop() || '').toLowerCase();
  if (['js', 'mjs', 'cjs', 'jsx', 'ts', 'tsx', 'json'].includes(ext)) return JS_RULES;
  if (ext === 'css') return CSS_RULES;
  if (['html', 'htm', 'xml', 'svg', 'vue'].includes(ext)) return HTML_RULES;
  return null;
}

// tokenize splits code into {text, cls} segments using the rule set; cls is null
// for the text between matches. Guards against a zero-width match looping.
export function tokenize(code, rules) {
  const re = new RegExp(rules.map(([, r]) => '(' + r.source + ')').join('|'), 'g');
  const out = [];
  let last = 0;
  let m;
  while ((m = re.exec(code)) !== null) {
    if (m.index > last) out.push({ text: code.slice(last, m.index), cls: null });
    let cls = null;
    for (let i = 0; i < rules.length; i++) {
      if (m[i + 1] !== undefined) { cls = rules[i][0]; break; }
    }
    out.push({ text: m[0], cls });
    last = m.index + m[0].length;
    if (m[0].length === 0) re.lastIndex++;
  }
  if (last < code.length) out.push({ text: code.slice(last), cls: null });
  return out;
}

// highlightHTML turns code into escaped, span-wrapped HTML for the highlight
// layer. With no rules (or on any tokenizer error) it returns plain escaped text.
export function highlightHTML(value, rules) {
  if (!rules) return escapeHtml(value);
  try {
    return tokenize(value, rules)
      .map((s) => (s.cls ? `<span class="tok-${s.cls}">${escapeHtml(s.text)}</span>` : escapeHtml(s.text)))
      .join('');
  } catch {
    return escapeHtml(value);
  }
}

/**
 * Create a local editor instance in `container`. `filename` selects the
 * highlighter by extension. Returns Promise<{getValue, setValue, destroy, view}>
 * (async to keep the call shape its importers already use).
 */
export async function createEditor(container, { value = '', filename = '', onChange, minHeight = 300, readOnly = false } = {}) {
  const rules = rulesFor(filename);

  const gutterInner = document.createElement('div');
  gutterInner.className = 'code-editor__gutter-inner';
  const gutter = document.createElement('div');
  gutter.className = 'code-editor__gutter';
  gutter.appendChild(gutterInner);

  const code = document.createElement('code');
  const pre = document.createElement('pre');
  pre.className = 'code-editor__highlight';
  pre.setAttribute('aria-hidden', 'true');
  pre.appendChild(code);

  const activeLine = document.createElement('div');
  activeLine.className = 'code-editor__active-line';

  const textarea = document.createElement('textarea');
  textarea.className = 'code-editor__input';
  textarea.value = value;
  textarea.spellcheck = false;
  textarea.setAttribute('autocomplete', 'off');
  textarea.setAttribute('autocapitalize', 'off');
  textarea.setAttribute('autocorrect', 'off');
  textarea.setAttribute('wrap', 'off');
  if (readOnly) textarea.readOnly = true;

  const area = document.createElement('div');
  area.className = 'code-editor__area';
  area.appendChild(activeLine);
  area.appendChild(pre);
  area.appendChild(textarea);

  const root = document.createElement('div');
  root.className = 'code-editor' + (readOnly ? ' code-editor--readonly' : '');
  root.style.minHeight = minHeight + 'px';
  root.appendChild(gutter);
  root.appendChild(area);

  let lineCount = -1;
  function renderGutter() {
    const n = (textarea.value.match(/\n/g) || []).length + 1;
    if (n === lineCount) return;
    lineCount = n;
    let s = '';
    for (let i = 1; i <= n; i++) s += i + '\n';
    gutterInner.textContent = s;
  }

  function renderHighlight() {
    // A lone trailing newline in a <pre> drops the final (empty) line's height; a
    // trailing space keeps the highlight layer as tall as the textarea.
    const v = textarea.value;
    code.innerHTML = highlightHTML(v.endsWith('\n') ? v + ' ' : v, rules);
  }

  function positionActiveLine() {
    if (readOnly) return;
    const before = textarea.value.slice(0, textarea.selectionStart);
    const line = (before.match(/\n/g) || []).length;
    activeLine.style.transform = `translateY(${PAD_TOP + line * LINE_HEIGHT - textarea.scrollTop}px)`;
  }

  function syncScroll() {
    pre.scrollTop = textarea.scrollTop;
    pre.scrollLeft = textarea.scrollLeft;
    gutterInner.style.transform = `translateY(${-textarea.scrollTop}px)`;
    positionActiveLine();
  }

  function refresh() {
    renderGutter();
    renderHighlight();
    positionActiveLine();
  }

  const onInput = () => {
    refresh();
    if (onChange) onChange(textarea.value);
  };
  const caretEvents = ['keyup', 'click', 'focus'];
  const onFocus = () => root.classList.add('is-focused');
  const onBlur = () => root.classList.remove('is-focused');

  // Tab inserts spaces instead of moving focus, matching a code editor. Shift+Tab
  // is left alone so a keyboard user is never trapped in the field.
  const onKeydown = (e) => {
    if (e.key !== 'Tab' || e.shiftKey || readOnly) return;
    e.preventDefault();
    let inserted = false;
    // insertText keeps the browser's undo history and fires 'input' (which
    // refreshes the view); fall back to a direct edit where it is unavailable.
    try { inserted = document.execCommand('insertText', false, INDENT); } catch { inserted = false; }
    if (!inserted) {
      textarea.setRangeText(INDENT, textarea.selectionStart, textarea.selectionEnd, 'end');
      refresh();
      if (onChange) onChange(textarea.value);
    }
  };

  textarea.addEventListener('input', onInput);
  textarea.addEventListener('scroll', syncScroll);
  caretEvents.forEach((e) => textarea.addEventListener(e, positionActiveLine));
  textarea.addEventListener('focus', onFocus);
  textarea.addEventListener('blur', onBlur);
  textarea.addEventListener('keydown', onKeydown);

  container.appendChild(root);
  refresh();

  return {
    getValue: () => textarea.value,
    setValue: (newValue) => {
      textarea.value = newValue;
      refresh();
      if (onChange) onChange(textarea.value);
    },
    destroy: () => {
      textarea.removeEventListener('input', onInput);
      textarea.removeEventListener('scroll', syncScroll);
      caretEvents.forEach((e) => textarea.removeEventListener(e, positionActiveLine));
      textarea.removeEventListener('focus', onFocus);
      textarea.removeEventListener('blur', onBlur);
      textarea.removeEventListener('keydown', onKeydown);
      root.remove();
    },
    view: textarea,
  };
}
