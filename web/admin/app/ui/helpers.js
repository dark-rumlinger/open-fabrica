/**
 * Shared UI helpers, keeps views DRY.
 */

import { h, clear } from '../core/dom.js';
import { t } from '../core/i18n.js';
import { icon } from './icon.js';
import { select } from '../core/state.js';
import * as toast from './toast.js';
import { humanizeError } from './errors.js';

/**
 * Build a labeled code-editor field for a modal: an editor container inside a
 * form-group, with the heavy code-editor lazy-loaded and a plain textarea
 * fallback if that import fails. Returns { form, getValue, destroy }; the caller
 * owns the modal and the save/fetch logic. Lives here (in ui/) so the dynamic
 * import('./code-editor.js') resolves relative to ui/ for every caller.
 */
export function buildCodeEditorField({ label, initial = '', filename, minHeight = 350 } = {}) {
  const editorContainer = h('div');
  let editor = null;

  const form = h('div', {}, [
    h('div', { className: 'form-group' }, [
      h('label', {}, label),
      editorContainer,
    ]),
  ]);

  import('./code-editor.js').then(async ({ createEditor }) => {
    editor = await createEditor(editorContainer, { value: initial, filename, minHeight });
  }).catch(() => {
    const fallback = h('textarea', {
      className: 'input',
      rows: minHeight > 350 ? 16 : 12,
      style: { fontFamily: 'var(--font-mono)', fontSize: 'var(--text-sm)', tabSize: '2' },
      value: initial,
    });
    editorContainer.appendChild(fallback);
    editor = { getValue: () => fallback.value, destroy: () => {} };
  });

  return {
    form,
    getValue: () => (editor ? editor.getValue() : initial),
    destroy: () => { if (editor) editor.destroy(); },
  };
}

/**
 * Fetch-then-render with the standard error toast, for multi-section views
 * that can't use createListView. Returns true on success, false if the fetch
 * threw (so callers can early-out). Pass `container` to also leave an error
 * state with a Retry button in it instead of a blank panel; Retry re-runs the
 * whole load.
 */
export async function loadInto(fetchFn, render, { errorMsg = t('list.loadFailed'), container } = {}) {
  let data;
  try {
    data = await fetchFn();
  } catch (err) {
    const reason = humanizeError(err.message);
    if (container) {
      clear(container);
      container.appendChild(errorState({
        message: `${errorMsg}: ${reason}`,
        retry: () => loadInto(fetchFn, render, { errorMsg, container }),
        reference: err.requestId,
      }));
    } else {
      toast.error(errorMsg + ': ' + reason);
    }
    return false;
  }
  if (container) clear(container);
  render(data);
  return true;
}

/**
 * Error state with an optional Retry button. Use it wherever a load failure
 * would otherwise leave a blank panel.
 *   errorState({ message: 'Failed to load files: ...', retry: () => load() })
 */
export function errorState({ title = t('helpers.wentWrong'), message, retry, reference } = {}) {
  const node = emptyState({
    iconName: 'alert-triangle',
    title,
    description: message,
    action: retry ? { label: t('common.tryAgain'), kind: 'secondary', onClick: retry } : undefined,
  });
  node.classList.add('empty-state--error');
  node.setAttribute('role', 'alert');
  // A request reference, when the failure carried one: click to copy it into a
  // bug report. It correlates with the X-Request-ID header and the server log.
  if (reference) {
    node.appendChild(clickable(
      h('p', { className: 'error-state__ref', title: t('errors.copyReference') },
        t('errors.reference', { id: reference })),
      () => copyText(reference, t('errors.referenceCopied')),
    ));
  }
  return node;
}

/**
 * Replace a panel's content with a load-failure state and a Retry button.
 * For views that fetch outside createListView/loadInto:
 *   catch (err) { showLoadError(container, 'Failed to load files', err, load); return; }
 */
export function showLoadError(container, label, err, retry) {
  clear(container);
  container.appendChild(errorState({
    message: `${label}: ${humanizeError(err && err.message !== undefined ? err.message : err)}`,
    retry,
    reference: err && err.requestId,
  }));
}

/**
 * Copy text to the clipboard with a toast either way. The Clipboard API is
 * missing on plain-http non-localhost origins and rejects when permission is
 * denied, so it falls back to a hidden textarea + execCommand and only warns
 * if that fails too.
 */
export async function copyText(text, successMsg = t('helpers.copied')) {
  try {
    await navigator.clipboard.writeText(text);
    toast.success(successMsg);
    return;
  } catch { /* fall through to the legacy path */ }
  const ta = h('textarea', {
    readonly: true,
    'aria-hidden': 'true',
    style: { position: 'fixed', top: '-1000px', opacity: '0' },
  });
  ta.value = text;
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch { ok = false; }
  ta.remove();
  if (ok) toast.success(successMsg);
  else toast.warning(t('helpers.copyFailed'));
}

/**
 * Reload a project view when a matching tool run succeeds. A given `match`
 * list (tools/actions) must include the event's tool / args.action; omit a
 * list to accept any. `reload` is called with the event.
 */
export function watchToolReload(scope, projectId, match, reload) {
  select('toolExecuted', (evt) => {
    if (evt.project_id !== projectId || !evt.success) return;
    if (match.tools && !match.tools.includes(evt.tool)) return;
    if (match.actions && !match.actions.includes(evt.args?.action)) return;
    reload(evt);
  }, scope);
}

let formFieldSeq = 0;

/**
 * Standard form-group: label (with optional required mark) + input + optional hint.
 *   formGroup('Username', input, { required: true, hint: 'min 8 chars' })
 * The label is tied to the control (for/id) so clicking it focuses the field
 * and screen readers announce it; the hint is wired up via aria-describedby.
 */
export function formGroup(label, input, opts = {}) {
  const control = (input && ['INPUT', 'SELECT', 'TEXTAREA'].includes(input.tagName))
    ? input
    : (input && input.querySelector ? input.querySelector('input, select, textarea') : null);
  const labelAttrs = {};
  if (control) {
    if (!control.id) control.id = `field-${++formFieldSeq}`;
    labelAttrs.for = control.id;
  }
  const labelChildren = [label];
  if (opts.required) labelChildren.push(h('span', { className: 'required' }, ' *'));
  const children = [h('label', labelAttrs, labelChildren), input];
  if (opts.hint) {
    const hint = h('p', { className: 'form-hint' }, opts.hint);
    if (control) {
      hint.id = `${control.id}-hint`;
      control.setAttribute('aria-describedby', hint.id);
    }
    children.push(hint);
  }
  return h('div', { className: 'form-group' }, children);
}

/**
 * Show/hide button for a password input. Keyboard reachable and announced:
 * the label and pressed state follow the field.
 *   h('div', { className: 'password-wrapper' }, [input, passwordToggle(input)])
 */
export function passwordToggle(input, noun = 'password') {
  // `noun` is 'password' or 'password confirmation'; each has whole-sentence
  // labels so word order can follow the language.
  const keys = noun === 'password'
    ? { show: 'password.show', hide: 'password.hide' }
    : { show: 'password.showConfirm', hide: 'password.hideConfirm' };
  let visible = false;
  const btn = h('button', {
    className: 'password-toggle',
    type: 'button',
    innerHTML: icon('eye-off'),
    'aria-label': t(keys.show),
    'aria-pressed': false,
    onClick: (e) => {
      e.preventDefault();
      visible = !visible;
      input.type = visible ? 'text' : 'password';
      btn.innerHTML = icon(visible ? 'eye' : 'eye-off');
      btn.setAttribute('aria-label', t(visible ? keys.hide : keys.show));
      btn.setAttribute('aria-pressed', String(visible));
    },
  });
  return btn;
}

/**
 * Read-only form group: label + a monospace, non-editable value. Used by edit
 * modals to show the immutable identifier (a secret's name, a memory's key)
 * above the editable fields.
 */
export function readonlyField(label, value) {
  return h('div', { className: 'form-group' }, [
    h('label', {}, label),
    h('div', { className: 'text-sm', style: { fontFamily: 'var(--font-mono)', padding: '6px 0' } }, value),
  ]);
}

/**
 * Empty state.
 *   emptyState({ title?, description?, iconName?, action? })
 *     action: { label, onClick, kind? }
 */
export function emptyState(opts = {}) {
  const { title, description, iconName, action, secondaryAction } = opts;
  const children = [];
  if (iconName) {
    children.push(h('div', { className: 'empty-state__icon-wrap' }, [
      h('div', { className: 'empty-state__icon', innerHTML: icon(iconName) }),
    ]));
  }
  if (title) {
    children.push(h('h4', { className: 'empty-state__title' }, title));
  }
  if (description) {
    children.push(h('p', { className: 'empty-state__desc' }, description));
  }
  if (action && action.label) {
    children.push(h('button', {
      className: `btn btn--${action.kind || 'primary'} btn--lg empty-state__action`,
      onClick: action.onClick,
    }, action.label));
  }
  if (secondaryAction && secondaryAction.label) {
    children.push(h('button', {
      className: 'btn btn--secondary empty-state__action',
      onClick: secondaryAction.onClick,
    }, secondaryAction.label));
  }
  return h('div', { className: 'empty-state' }, children);
}

/**
 * Stacked card skeletons for a list/CRUD view that's still loading. Reuses the
 * shared .skeleton classes so every list reads as "loading" the same way the
 * dashboard does, instead of a bare "Loading…" line.
 * @param {number} [count] number of placeholder cards.
 */
export function listSkeleton(count = 3) {
  const cards = [];
  for (let i = 0; i < count; i++) {
    cards.push(h('div', { className: 'skeleton skeleton--card' }));
  }
  return h('div', { className: 'skeleton-list' }, cards);
}

/**
 * Update a live stat value, with a brief highlight when it actually changed.
 * An unchanged value is left alone so a periodic refresh stays quiet.
 * @param {HTMLElement|null} el a `.stat-card__value` element.
 * @param {string} text the new value.
 */
export function setStatValue(el, text) {
  if (!el || el.textContent === text) return;
  el.textContent = text;
  // Remove, force a reflow, re-add: restarts the animation on rapid changes.
  el.classList.remove('is-updated');
  void el.offsetWidth;
  el.classList.add('is-updated');
}

/**
 * Make a non-button element behave like a button: pointer cursor, keyboard
 * focus, and Enter/Space activation alongside click. Returns the same
 * element, for chaining.
 */
export function clickable(el, onClick) {
  el.classList.add('cursor-pointer');
  el.setAttribute('role', 'button');
  el.setAttribute('tabindex', '0');
  el.addEventListener('click', onClick);
  el.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      onClick(e);
    }
  });
  return el;
}

// Human-readable byte size, in the interface language (core/format.js).
export { formatBytes } from '../core/format.js';

/**
 * Turn plain text into a child list where every http(s) URL is a clickable
 * link opening in a new tab. Returns an array of strings and anchor elements
 * for h()'s children. Trailing sentence punctuation stays outside the link.
 *   h('p', {}, linkifyUrls('Working at http://a.localhost:5733, but ...'))
 */
export function linkifyUrls(text) {
  if (typeof text !== 'string' || !text) return [text];
  const parts = [];
  const re = /https?:\/\/[^\s]+/g;
  let last = 0;
  let m;
  while ((m = re.exec(text)) !== null) {
    const url = m[0].replace(/[.,;:!?)]+$/, '');
    if (m.index > last) parts.push(text.slice(last, m.index));
    parts.push(h('a', { href: url, target: '_blank', rel: 'noopener', className: 'link' }, url));
    if (url.length < m[0].length) parts.push(m[0].slice(url.length));
    last = m.index + m[0].length;
  }
  if (last < text.length) parts.push(text.slice(last));
  return parts;
}

/**
 * True for a real, internet-routable domain name (not localhost/*.localhost
 * and not an IP literal). Mirrors the backend's DomainKind classification.
 */
export function isPublicDomain(domain) {
  if (!domain) return false;
  if (domain === 'localhost' || domain.endsWith('.localhost')) return false;
  if (/^[0-9.]+$/.test(domain) || domain.includes(':')) return false;
  return domain.includes('.');
}

/**
 * Build a public-facing {url, label} for a project. Real domains link as
 * https:// (the TLS proxy redirects HTTP) unless the instance reports TLS
 * disabled; local and IP domains stay plain HTTP on the public port.
 */
export function formatPublicUrl(project, systemStatus) {
  if (!project.domain) {
    return { url: '#', label: t('helpers.noDomain') };
  }
  const host = project.domain;
  const publicPort = (systemStatus && systemStatus.public_port) || 80;
  if (isPublicDomain(host)) {
    const tlsDisabled = !!(systemStatus && systemStatus.tls_disabled);
    return tlsDisabled
      ? { url: `http://${host}`, label: host }
      : { url: `https://${host}`, label: host };
  }
  return { url: `http://${host}:${publicPort}`, label: `${host}:${publicPort}` };
}

/**
 * The origin visitors reach this project at, for building copyable URLs. The
 * admin panel's own origin is wrong for that: it is a different host or port
 * than the public site. Falls back to the admin origin when the project has no
 * domain yet.
 */
export function publicOrigin(project, systemStatus) {
  if (!project || !project.domain) return window.location.origin;
  return formatPublicUrl(project, systemStatus).url;
}

/** Human-readable duration from seconds. */
export function formatInterval(seconds) {
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.round(seconds / 60)}m`;
  if (seconds < 86400) return `${Math.round(seconds / 3600)}h`;
  return `${Math.round(seconds / 86400)}d`;
}

/**
 * Segmented control: a labelled row of mutually-exclusive options.
 *   segmented([{ value: 'off', label: 'Off' }, ...], 'medium', onChange, 'Detail')
 * Read the current selection via the returned element's `_value` property.
 */
export function segmented(options, value, onChange, label) {
  let current = value;
  const buttons = [];
  const group = h('div', { className: 'segmented', role: 'radiogroup' });

  if (label) group.setAttribute('aria-label', label);

  // Roving tabindex: only the checked option is a tab stop, so Tab enters and
  // leaves the group as one control and arrow keys move within it.
  function select(v) {
    current = v;
    buttons.forEach((b) => {
      const active = b.dataset.value === current;
      b.classList.toggle('is-active', active);
      b.setAttribute('aria-checked', String(active));
      b.tabIndex = active ? 0 : -1;
    });
  }

  // Arrow keys move the selection through the group and wrap, like a native
  // radio set; selection follows focus, so onChange fires as it moves.
  function move(delta) {
    const at = buttons.findIndex((b) => b.dataset.value === current);
    const next = buttons[(Math.max(at, 0) + delta + buttons.length) % buttons.length];
    if (!next || next.dataset.value === current) return;
    select(next.dataset.value);
    next.focus();
    if (onChange) onChange(current);
  }

  options.forEach((opt) => {
    const btn = h('button', { type: 'button', role: 'radio', className: 'segmented__option' }, opt.label);
    btn.dataset.value = opt.value;
    btn.addEventListener('click', () => {
      if (current === opt.value) return;
      select(opt.value);
      if (onChange) onChange(current);
    });
    buttons.push(btn);
    group.appendChild(btn);
  });

  group.addEventListener('keydown', (e) => {
    const keys = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1 };
    if (!(e.key in keys)) return;
    e.preventDefault();
    move(keys[e.key]);
  });

  select(current);

  const root = label
    ? h('div', { className: 'segmented-field' }, [
      h('label', { className: 'segmented-field__label' }, label),
      group,
    ])
    : group;
  Object.defineProperty(root, '_value', { get() { return current; } });
  return root;
}

/** Human-readable description of a cron expression. */
export function describeCron(expr) {
  if (!expr) return '';
  const parts = expr.trim().split(/\s+/);
  if (parts.length !== 5) return expr;
  const [min, hour, dom, mon, dow] = parts;
  if (min === '*' && hour === '*') return t('cron.everyMinute');
  if (min.startsWith('*/')) return t('cron.everyMinutes', { n: min.slice(2) });
  if (hour.startsWith('*/') && min === '0') return t('cron.everyHours', { n: hour.slice(2) });
  if (dom === '*' && mon === '*') {
    const time = `${hour.padStart(2, '0')}:${min.padStart(2, '0')}`;
    if (dow === '*' && hour !== '*' && min !== '*') return t('cron.daily', { time });
    if (dow === '1-5') return t('cron.weekdays', { time });
    if (dow === '0,6') return t('cron.weekends', { time });
    if (dow === '0' || dow === '7') return t('cron.sundays', { time });
    if (dow === '1') return t('cron.mondays', { time });
  }
  return expr;
}
