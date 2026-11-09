/**
 * Chat message component.
 */

import { h } from '../../core/dom.js';
import { t, tn } from '../../core/i18n.js';
import { render as renderMarkdown } from '../../core/markdown.js';
import { icon } from '../icon.js';
import { disclosure } from '../a11y.js';
import { copyText } from '../helpers.js';
import { displayName } from '../../core/state.js';
import { createAvatar } from './avatar.js';
import { createGoLiveCard } from './cards/go-live-card.js';

// createToolCall now lives in cards/tool-call.js; re-exported so the chat view's
// existing import from message.js keeps working.
export { createToolCall } from './cards/tool-call.js';
import { stageDividerLabel, stageIcon } from './stage-copy.js';
import { timestamp } from './timestamp.js';

/**
 * Create a user message element.
 * @param {string} text
 * @param {string} [createdAt], ISO timestamp from DB history.
 */
export function createUserMessage(text, createdAt) {
  const name = displayName() || t('chat.you');
  const badge = h('div', { className: 'message__user-badge' }, [
    createAvatar(name, 'user'),
    h('span', {}, name),
    timestamp(createdAt),
  ]);

  const msg = h('div', { className: 'message message--user' }, [
    badge,
    h('div', { className: 'message__content' }, text),
  ]);
  return msg;
}

/**
 * Create an assistant message element. Content is rendered as markdown.
 * @param {string} text
 * @param {string} [createdAt], ISO timestamp from DB history.
 */
export function createAssistantMessage(text, createdAt) {
  const content = h('div', { className: 'message__content' });
  content.innerHTML = renderMarkdown(text);
  const msg = h('div', { className: 'message message--assistant' }, [
    content,
    timestamp(createdAt),
    copyMessageButton(text),
  ]);
  return msg;
}

/**
 * A small "copy" control on a reply: shown on hover or keyboard focus, copies the
 * reply's original text (the markdown source, so code blocks paste cleanly).
 */
function copyMessageButton(text) {
  return h('button', {
    className: 'message__copy',
    type: 'button',
    title: t('chat.copyReply'),
    'aria-label': t('chat.copyReply'),
    innerHTML: icon('copy'),
    onClick: () => copyText(text, t('helpers.copied')),
  });
}

/**
 * Create a streaming assistant message that can be updated.
 * Shows a 3-dot typing indicator until the first token arrives.
 * @returns {{ element: HTMLElement, appendText: Function, finish: Function }}
 */
export function createStreamingMessage() {
  const msg = h('div', { className: 'message message--assistant' });
  // Avatar plus dots, nothing else: the dots already say "working", so no
  // rotating copy sits beside them to carry information they do not.
  const indicator = h('div', { className: 'thinking-row' }, [
    createAvatar('Open Fabrica', 'brain'),
    h('div', { className: 'typing-indicator' }, [
      h('span', { className: 'typing-dot' }),
      h('span', { className: 'typing-dot' }),
      h('span', { className: 'typing-dot' }),
    ]),
  ]);
  const cursor = h('span', { className: 'typing-cursor' });
  msg.appendChild(indicator);

  let buffer = '';
  let hasFirstToken = false;

  function appendText(text) {
    buffer += text;
    if (!hasFirstToken) {
      hasFirstToken = true;
      if (indicator.parentNode) indicator.remove();
    }
    msg.innerHTML = renderMarkdown(buffer);
    msg.appendChild(cursor);
  }

  function finish() {
    if (indicator.parentNode) indicator.remove();
    if (cursor.parentNode) cursor.remove();
    // If no content was streamed, remove the empty bubble entirely.
    if (!buffer.trim()) {
      msg.remove();
      return;
    }
    const content = h('div', { className: 'message__content' });
    content.innerHTML = renderMarkdown(buffer);
    msg.innerHTML = '';
    msg.appendChild(content);
    msg.appendChild(timestamp());
    msg.classList.add('message--just-finished');
    setTimeout(() => msg.classList.remove('message--just-finished'), 800);
  }

  return { element: msg, appendText, finish };
}

/**
 * Create an automated system message element. Used for engine-generated "user"
 * role messages (stage instructions) that the human didn't actually type.
 * @param {string} text
 * @param {string} [createdAt], ISO timestamp from DB history.
 */
export function createAutomatedMessage(text, createdAt) {
  const badge = h('div', { className: 'message__system-badge' }, [
    h('span', { innerHTML: icon('zap') }),
    h('span', {}, displayName() || t('chat.you')),
    h('span', { className: 'message__auto-label' }, t('chat.automated')),
    timestamp(createdAt),
  ]);

  const msg = h('div', { className: 'message message--automated' }, [
    badge,
    h('div', { className: 'message__content' }, text),
  ]);
  return msg;
}

// Leading-line markers the engine emits so the chat can give certain stage
// outputs a richer treatment than a plain markdown bubble. These strings are a
// contract with the backend (the engine's EXPAND and live-summary output) and
// must match the text it prepends, byte for byte (engine.ExpandedBriefMarker is
// pinned to this file by a test). The marker is stripped and the card's title
// comes from the catalog, so it is never shown to anyone.
const BRAIN_EXPAND_MARKER = '**Expanded brief**';
// No current build writes this one. It is kept so plan summaries saved in
// older projects still get their panel styling.
const BRAIN_PLAN_MARKER = "**Here's the plan**";

// Trailing marker the engine appends to the go-live message: a base64-encoded
// goLivePayload inside an HTML comment (see live_summary.go goLiveMarker). It's a
// byte-for-byte contract with the backend. Match everything up to the closing
// `-->` (base64's alphabet [A-Za-z0-9+/=] never contains `-`, so the first `-->`
// is always the real terminator); a malformed payload then fails the base64
// decode and falls back cleanly, rather than leaking the marker as visible text.
const GOLIVE_MARKER_RE = /\n*<!--of-golive:([\s\S]*?)-->\s*$/;

// Decode a base64 string that holds UTF-8 bytes (project/table names may be
// non-ASCII, so atob alone - which yields Latin-1 - would corrupt them).
function decodeBase64Utf8(b64) {
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return new TextDecoder().decode(bytes);
}

/**
 * Detect and peel the go-live marker off a brain message. Returns null when the
 * message carries no marker. Otherwise returns the marker-stripped markdown
 * (always) plus the parsed payload (null if the base64/JSON didn't parse), so
 * the caller can render the card when possible and fall back to clean markdown
 * otherwise - the marker never leaks into the rendered text.
 */
function parseGoLiveMarker(text) {
  const m = text.match(GOLIVE_MARKER_RE);
  if (!m) return null;
  const markdown = text.slice(0, m.index).replace(/\s+$/, '');
  let payload = null;
  try {
    payload = JSON.parse(decodeBase64Utf8(m[1]));
  } catch {
    // Corrupt/partial payload: fall back to the markdown, never throw.
  }
  return { markdown, payload };
}

/**
 * Create an engine message element (assistant styling + engine badge). The
 * EXPAND brief renders as a collapsible card, the PLAN overview gets a panel
 * accent, everything else is plain markdown.
 */
export function createBrainMessage(text, createdAt) {
  const badge = h('div', { className: 'message__brain-badge' }, [
    createAvatar('Open Fabrica', 'brain'),
    h('span', {}, 'Open Fabrica'),
    timestamp(createdAt),
  ]);

  // Go-live summary: render the structured card when the payload is present and
  // parses, else strip the marker and render the plain markdown that precedes it.
  if (typeof text === 'string') {
    const gl = parseGoLiveMarker(text);
    if (gl) {
      if (gl.payload && gl.payload.kind === 'golive') {
        const card = createGoLiveCard(badge, gl.payload);
        if (card) return card;
      }
      text = gl.markdown;
    }
  }

  if (typeof text === 'string' && text.startsWith(BRAIN_EXPAND_MARKER)) {
    const prose = text.slice(BRAIN_EXPAND_MARKER.length).trim();
    return createExpandBriefMessage(badge, prose);
  }

  const body = h('div', { className: 'message__body' });
  body.innerHTML = renderMarkdown(text);

  const msg = h('div', { className: 'message message--brain' }, [badge, body]);
  if (typeof text === 'string' && text.startsWith(BRAIN_PLAN_MARKER)) {
    msg.classList.add('message--plan');
  }
  return msg;
}

/**
 * Fill a build-progress bar the first time its card scrolls into view so the
 * width transition runs as a reveal. One-shot: the observer disconnects after
 * firing. prefers-reduced-motion or a missing IntersectionObserver snaps
 * straight to the final width.
 */
function revealProgress(card, fill, pct) {
  const target = `${pct}%`;
  const reduce = window.matchMedia
    && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  if (reduce || typeof IntersectionObserver === 'undefined') {
    fill.style.width = target;
    return;
  }
  const observer = new IntersectionObserver((entries) => {
    if (!entries.some(e => e.isIntersecting)) return;
    fill.style.width = target;
    observer.disconnect();
  }, { threshold: 0.2 });
  observer.observe(card);
}

/**
 * Create a build-progress card: a snapshot of the queue when task(s)
 * complete, with a progress bar and the next remaining tasks. App-composed
 * DOM rather than markdown because the queue state is structured. Rendered
 * once and never mutated: a later completion appends a fresh card, so the
 * feed keeps an honest running history.
 */
export function createBuildProgressCard({ completed, doneCount, totalCount, remaining, createdAt }) {
  const badge = h('div', { className: 'message__brain-badge' }, [
    createAvatar('Open Fabrica', 'brain'),
    h('span', {}, 'Open Fabrica'),
    timestamp(createdAt),
  ]);

  const doneTitle = completed.join(', ');
  const doneSub = tn('chat.tasksComplete', completed.length);
  const banner = h('div', { className: 'build-progress__done' }, [
    h('span', { className: 'build-progress__check', innerHTML: icon('check') }),
    h('div', { className: 'build-progress__done-text' }, [
      h('div', { className: 'build-progress__done-title' }, doneTitle),
      h('div', { className: 'build-progress__done-sub' }, doneSub),
    ]),
  ]);

  const pct = totalCount > 0 ? Math.round((doneCount / totalCount) * 100) : 0;
  // Start empty; revealProgress() fills to `pct` the first time the card is
  // scrolled into view so the bar visibly animates rather than rendering
  // pre-filled. The width transition itself lives on .progress-bar__fill.
  const progressFill = h('div', { className: 'progress-bar__fill', style: { width: '0%' } });
  const progress = h('div', { className: 'build-progress__progress' }, [
    h('div', { className: 'build-progress__meta' }, [
      h('span', {}, t('chat.buildProgress')),
      h('span', {}, [
        h('span', { className: 'build-progress__count' }, String(doneCount)),
        ' ' + tn('chat.ofTasks', totalCount),
      ]),
    ]),
    h('div', { className: 'progress-bar' }, [progressFill]),
  ]);

  const children = [banner, progress];

  if (remaining.length) {
    // Show only the immediate horizon (first is "up next", the rest are queued)
    // and collapse the tail behind a "+N more" toggle so a long backlog doesn't
    // dominate the card - the tail rows are rendered but hidden until expanded.
    const MAX = 4;
    const rows = remaining.map((title, i) => h('div', {
      className: 'build-progress__task'
        + (i === 0 ? ' build-progress__task--next' : '')
        + (i >= MAX ? ' build-progress__task--extra' : ''),
    }, [
      i === 0
        ? h('span', { className: 'build-progress__task-icon', innerHTML: icon('play') })
        : h('span', { className: 'build-progress__dot' }),
      h('span', { className: 'build-progress__task-name' }, title),
    ]));
    const list = h('div', { className: 'build-progress__list' }, rows);

    const nextChildren = [
      h('div', { className: 'build-progress__label' }, t('chat.upNext')),
      list,
    ];
    const extra = remaining.length - MAX;
    if (extra > 0) {
      const collapsedLabel = tn('chat.moreTasks', extra);
      const more = h('div', {
        className: 'build-progress__more build-progress__more--toggle',
        role: 'button',
        tabindex: '0',
      }, collapsedLabel);
      const toggle = () => {
        const open = list.classList.toggle('build-progress__list--expanded');
        more.textContent = open ? t('chat.showFewer') : collapsedLabel;
      };
      more.addEventListener('click', toggle);
      more.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          toggle();
        }
      });
      nextChildren.push(more);
    }
    children.push(h('div', { className: 'build-progress__next' }, nextChildren));
  } else {
    children.push(h('div', { className: 'build-progress__all-done' }, [
      h('span', { className: 'build-progress__all-done-icon', innerHTML: icon('sparkle') }),
      h('span', {}, t('chat.allDone')),
    ]));
  }

  const body = h('div', { className: 'message__body' }, [
    h('div', { className: 'build-progress' }, children),
  ]);
  const card = h('div', { className: 'message message--brain message--build' }, [badge, body]);
  revealProgress(card, progressFill, pct);
  return card;
}

// The engine joins a task's name and its detail with a dash separator (an em
// dash, written here as an escape, or a plain hyphen). Either splits the title.
const TASK_TITLE_SEPARATORS = [' \u2014 ', ' - '];

/**
 * Split a task title on its first dash separator into a bold lead and a muted
 * detail tail. Long build-task names ("Renderer: canvas draw, ghost piece, ...")
 * read far better as lead + detail than as one flat run. Returns [lead, detail],
 * where detail is "" when the title carries no separator.
 */
function splitTaskTitle(title) {
  let at = -1;
  let len = 0;
  for (const sep of TASK_TITLE_SEPARATORS) {
    const i = title.indexOf(sep);
    if (i !== -1 && (at === -1 || i < at)) { at = i; len = sep.length; }
  }
  if (at === -1) return [title, ''];
  return [title.slice(0, at), title.slice(at + len)];
}

/**
 * Create a build-plan card, shown once at the start of a build when the full
 * task queue is first known. Companion to createBuildProgressCard (same frame
 * and row rhythm), every task pending, first flagged "Up next". Rendered once
 * and never mutated; completions arrive as build-progress cards.
 */
export function createBuildPlanCard({ tasks, createdAt }) {
  const badge = h('div', { className: 'message__brain-badge' }, [
    createAvatar('Open Fabrica', 'brain'),
    h('span', {}, 'Open Fabrica'),
    timestamp(createdAt),
  ]);

  const n = tasks.length;
  const header = h('div', { className: 'build-plan__header' }, [
    h('span', { className: 'build-plan__icon', innerHTML: icon('list') }),
    h('div', {}, [
      h('div', { className: 'build-plan__title' }, t('chat.buildPlan')),
      h('div', { className: 'build-plan__sub' }, tn('chat.tasksQueued', n)),
    ]),
  ]);

  const rows = tasks.map((title, i) => {
    const [lead, detail] = splitTaskTitle(title);
    const isNext = i === 0;
    const chip = isNext
      ? h('span', { className: 'build-plan__chip build-plan__chip--next', innerHTML: icon('play') })
      : h('span', { className: 'build-plan__chip' }, String(i + 1));
    const name = h('span', { className: 'build-plan__name' }, [
      h('span', { className: 'build-plan__name-lead' }, lead),
      detail ? h('span', { className: 'build-plan__name-detail' }, `: ${detail}`) : null,
    ].filter(Boolean));
    const children = [chip, name];
    if (isNext) children.push(h('span', { className: 'build-plan__flag' }, t('chat.upNext')));
    return h('div', {
      className: 'build-plan__task' + (isNext ? ' build-plan__task--next' : ''),
    }, children);
  });

  const body = h('div', { className: 'message__body' }, [
    h('div', { className: 'build-plan' }, [
      header,
      h('div', { className: 'build-plan__list' }, rows),
    ]),
  ]);
  return h('div', { className: 'message message--brain message--build-plan' }, [badge, body]);
}

/**
 * Render the EXPAND-stage brief as a collapsible card so the (long) prose stays
 * out of the way until the owner wants to read it. Collapsed by default.
 */
function createExpandBriefMessage(badge, prose) {
  const chevron = h('span', { className: 'expand-brief__chevron', innerHTML: icon('chevron-right') });
  const hint = h('span', { className: 'expand-brief__hint' }, t('chat.showDetails'));
  const header = h('div', { className: 'expand-brief__header' }, [
    chevron,
    h('span', { className: 'expand-brief__icon', innerHTML: icon('file-text') }),
    h('span', { className: 'expand-brief__title' }, t('chat.expandedBrief')),
    hint,
  ]);

  const body = h('div', { className: 'expand-brief__body message__body' });
  body.innerHTML = renderMarkdown(prose);

  const card = h('div', { className: 'expand-brief' }, [header, body]);
  disclosure(header, () => {
    const open = card.classList.toggle('expand-brief--open');
    hint.textContent = open ? t('chat.hideDetails') : t('chat.showDetails');
    return open;
  });

  return h('div', { className: 'message message--brain message--brief' }, [badge, card]);
}

/**
 * Create a stage transition divider. The raw stage code (EXPAND/PLAN/...) is
 * mapped to plain language via stage-copy.js so the user never sees a
 * codename.
 */
export function createStageTransition(stageName, duration) {
  const label = stageDividerLabel(stageName);
  const labelChildren = [
    h('span', { className: 'stage-transition__icon', innerHTML: icon(stageIcon(stageName)) }),
    h('span', {}, label),
  ];
  if (duration) {
    labelChildren.push(h('span', { className: 'stage-transition__dur' }, `· ${duration}`));
  }
  return h('div', { className: `stage-transition stage-transition--${String(stageName).toLowerCase()}` }, [
    h('div', { className: 'stage-transition__line' }),
    h('div', { className: 'stage-transition__label' }, labelChildren),
    h('div', { className: 'stage-transition__line' }),
  ]);
}

/**
 * Create a "welcome back" divider, shown when an owner returns to a project
 * that already has chat history. Subtle and dismissible (click to remove).
 * Reuses the stage-transition layout so it sits naturally in the feed.
 * @param {string} [name], Owner's first name.
 */
export function createWelcomeBack(name) {
  const label = name ? t('chat.welcomeBackNamed', { name }) : t('chat.welcomeBack');
  const el = h('div', { className: 'stage-transition chat-welcome' }, [
    h('div', { className: 'stage-transition__line' }),
    h('div', { className: 'stage-transition__label' }, [
      h('span', { innerHTML: icon('sparkle') }),
      h('span', {}, label),
    ]),
    h('div', { className: 'stage-transition__line' }),
  ]);
  el.title = t('common.dismiss');
  el.addEventListener('click', () => el.remove());
  return el;
}

/**
 * Create a celebratory milestone divider (e.g. when a project goes live).
 * Pure-CSS glow, SVG icon only.
 * @param {string} text, The milestone message.
 */
export function createMilestone(text) {
  return h('div', { className: 'chat-milestone' }, [
    h('span', { className: 'chat-milestone__icon', innerHTML: icon('sparkle') }),
    h('span', { className: 'chat-milestone__text' }, text),
  ]);
}

