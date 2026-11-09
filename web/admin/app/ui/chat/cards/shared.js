/**
 * Shared utilities used by every chat card type: label maps, panel-switch
 * plumbing, status-badge helpers, and the collapsible-header pattern.
 * Per-card-type renderers live alongside this file in cards/*.js.
 */

import { h } from '../../../core/dom.js';
import { t, hasKey } from '../../../core/i18n.js';
import { icon } from '../../icon.js';
import { disclosure } from '../../a11y.js';
import { setPanelSwitcher as setQuestionPanelSwitcher } from '../../question-cards.js';

let _switchPanel = null;

export function setPanelSwitcher(fn) {
  _switchPanel = fn;
  setQuestionPanelSwitcher(fn);
}

export function switchPanel(panel) {
  if (_switchPanel) _switchPanel(panel);
}

// Tool labels live in the locale catalogs: 'tool.<toolName>.<action>' for an
// action-level label and 'tool.<toolName>' as the fallback when no action is
// known. manage_files action labels render only via the generic-card fallback,
// i.e. exactly when args.kind is unknown, so per-call actions must stay
// kind-NEUTRAL ("Saving", not "Saving file"); the path already shows as the
// card subtitle. attach/detach/list_page_files are genuinely kind=file-only.

/** Friendly label for a tool, optionally using the action from args. */
export function getToolLabel(toolName, args) {
  if (args?.action) {
    const actionKey = `tool.${toolName}.${args.action}`;
    if (hasKey(actionKey)) return t(actionKey);
  }
  const key = `tool.${toolName}`;
  return hasKey(key) ? t(key) : toolName.replace(/_/g, ' ');
}

// kindMeta drives kind-specific behavior for artifact cards. The copy lives in
// the catalogs as 'art.kind.<kind>', 'art.view.<kind>' and 'art.act.<action>.<kind>'.
export const kindMeta = {
  page:      { icon: 'file-text', panel: 'pages'   },
  layout:    { icon: 'layers',    panel: 'layouts' },
  component: { icon: 'copy',      panel: 'layouts-components' },
};

/** Actions that have a kind-specific label ('art.act.<action>.<kind>'). */
export const artifactActions = ['save', 'patch', 'delete', 'rename', 'get', 'outline'];

const fileTypeMap = {
  image:  { ext: ['png','jpg','jpeg','gif','svg','webp','ico','bmp','avif'], icon: 'image', panel: 'files', label: 'image' },
  code:   { ext: ['js','ts','jsx','tsx','mjs','json','xml','yml','yaml','toml'], icon: 'code', panel: 'files', label: 'script' },
  style:  { ext: ['css','scss','sass','less'], icon: 'code', panel: 'files', label: 'stylesheet' },
  font:   { ext: ['woff','woff2','ttf','otf','eot'], icon: 'file', panel: 'files', label: 'font' },
  video:  { ext: ['mp4','webm','ogg','mov'], icon: 'file', panel: 'files', label: 'video' },
  audio:  { ext: ['mp3','wav','flac','aac','m4a'], icon: 'file', panel: 'files', label: 'audio' },
};

export function getFileType(filename) {
  const ext = (filename || '').split('.').pop()?.toLowerCase() || '';
  for (const type of Object.values(fileTypeMap)) {
    if (type.ext.includes(ext)) return type;
  }
  return { icon: 'file', panel: 'files', label: 'file' };
}

/** Update a card's status badge and apply the success/error flash. */
export function bindStatusUpdater(card) {
  return (newStatus) => {
    const badge = card.querySelector('.chat-card__header .badge:last-child');
    if (badge) {
      badge.className = newStatus === 'success' ? 'badge badge--success' : 'badge badge--danger';
      badge.textContent = newStatus === 'success' ? t('chat.done') : t('chat.error');
    }
    card.classList.remove('chat-card--running');
    const flashClass = newStatus === 'success' ? 'chat-card--success-flash' : 'chat-card--error-flash';
    card.classList.add(flashClass);
    setTimeout(() => card.classList.remove(flashClass), 700);
  };
}

/** Running / success / error pill rendered in the card header. */
export function createStatusBadge(status) {
  if (status === 'success') return h('span', { className: 'badge badge--success' }, t('chat.done'));
  if (status === 'error') return h('span', { className: 'badge badge--danger' }, t('chat.error'));
  return h('span', { className: 'badge badge--info' }, t('tasks.status.running'));
}

/**
 * Standard chat card: header (icon + label + status badge), collapsible body
 * and actions, wired to the shared status updater. Card-type renderers pass
 * only what differs; the skeleton lives here once.
 *   createChatCard({ variant: 'asset', iconName: 'link', label, status,
 *                    body: [...], actions: [panelLink(t('art.viewPages'), 'pages')] })
 */
export function createChatCard({ variant, iconName, label, status, body, actions }) {
  const card = h('div', { className: `chat-card chat-card--${variant}${status === 'running' ? ' chat-card--running' : ''}` }, [
    h('div', { className: 'chat-card__header' }, [
      h('span', { innerHTML: icon(iconName), className: `chat-card__icon chat-card__icon--${variant}` }),
      h('span', { className: 'chat-card__label' }, label),
      createStatusBadge(status),
    ]),
    h('div', { className: 'chat-card__body' }, body),
    h('div', { className: 'chat-card__actions' }, actions),
  ]);
  makeCollapsible(card);
  return { element: card, updateStatus: bindStatusUpdater(card) };
}

/** Ghost action button that jumps to an admin panel. */
export function panelLink(label, panel) {
  return h('button', {
    className: 'btn btn--ghost btn--sm',
    onClick: () => switchPanel(panel),
  }, label);
}

/**
 * Wrap a card with a collapsible header + detail section.
 * Auto-expands when status is 'running', collapsed otherwise.
 */
export function makeCollapsible(card) {
  const header = card.querySelector('.chat-card__header');
  if (!header) return;

  const chevron = h('span', { innerHTML: icon('chevron-right'), className: 'chat-card__chevron' });
  header.insertBefore(chevron, header.firstChild);

  const detail = h('div', { className: 'chat-card__detail' });
  const children = Array.from(card.children).filter(c => c !== header);
  for (const child of children) {
    detail.appendChild(child);
  }
  card.appendChild(detail);

  const badge = header.querySelector('.badge');
  const isRunning = badge && badge.classList.contains('badge--info');
  if (isRunning) {
    header.classList.add('expanded');
    detail.classList.add('visible');
  }

  disclosure(header, () => {
    header.classList.toggle('expanded');
    return detail.classList.toggle('visible');
  }, header.classList.contains('expanded'));
}
