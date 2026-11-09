/**
 * Artifact-family cards: page/layout/component, asset, link/unlink, and
 * the project-tree dump from manage_files action=project_tree.
 */

import { h } from '../../../core/dom.js';
import { t } from '../../../core/i18n.js';
import { icon } from '../../icon.js';
import {
  getToolLabel,
  kindMeta,
  artifactActions,
  getFileType,
  bindStatusUpdater,
  createStatusBadge,
  makeCollapsible,
  createChatCard,
  panelLink,
} from './shared.js';

export function createArtifactCard(toolName, status, args, result) {
  const kind = kindMeta[args?.kind] ? args.kind : 'page';
  const meta = kindMeta[kind];

  // args.file_path doubles as URL path (pages) or name (layouts/components).
  // The LLM sometimes sends a `title` too; show it as the primary label when
  // it's genuinely different from the path, otherwise just show the path
  // once, two identical strings next to each other read as "mainmain".
  const path = args?.file_path || '';
  const title = args?.title;
  const primary = title && title !== path ? title : path;
  const showPath = title && title !== path && path ? path : '';

  const actionLabel = artifactActions.includes(args?.action)
    ? t(`art.act.${args.action}.${kind}`)
    : getToolLabel(toolName, args);

  const bodyChildren = [h('strong', {}, primary || t(`art.kind.${kind}`))];
  if (showPath) {
    bodyChildren.push(h('code', { className: 'text-sm text-secondary ml-2' }, showPath));
  }

  return createChatCard({
    variant: 'page',
    iconName: meta.icon,
    label: actionLabel,
    status,
    body: bodyChildren,
    actions: [panelLink(t(`art.view.${kind}`), meta.panel)],
  });
}

export function createAssetCard(toolName, status, args, result) {
  // manage_files uses `file_path`; some media tools pass `filename` instead.
  const filename = args?.file_path || args?.filename || t('art.asset');
  const ft = getFileType(filename);

  return createChatCard({
    variant: 'asset',
    iconName: ft.icon,
    label: getToolLabel(toolName, args),
    status,
    body: [h('strong', {}, filename)],
    actions: [panelLink(t('art.viewFiles'), ft.panel)],
  });
}

// Surfaces the page⇄file wiring step, which otherwise hides inside a
// generic tool card and reads as "Managing files", useless context.
export function createLinkAssetCard(toolName, status, args, result) {
  const pagePath = args?.page_path || '?';
  const filePath = args?.file_path || '?';
  const isLink = args?.action === 'attach';

  return createChatCard({
    variant: 'asset',
    iconName: 'link',
    label: isLink ? t('art.attaching') : t('art.detaching'),
    status,
    body: [
      h('code', { className: 'text-sm' }, filePath),
      h('span', { className: 'text-secondary mx-2' }, isLink ? ' -> ' : ' ✕ '),
      h('code', { className: 'text-sm' }, pagePath),
    ],
    actions: [panelLink(t('art.viewPages'), 'pages')],
  });
}

// Renders manage_files action=project_tree results as a monospaced
// markdown block with a "Copy tree" button. The tool body already arrives
// formatted with line numbers and indentation that read fine in <pre>;
// rendering markdown on top would over-style headings into noise.
export function createProjectTreeCard(toolName, status, args, result) {
  let subtitle = '';
  try {
    if (args?.kinds) {
      const k = JSON.parse(args.kinds);
      if (Array.isArray(k) && k.length) subtitle = k.join(', ');
    }
  } catch { /* ignore */ }

  const treeBody = h('pre', { className: 'project-tree-card__tree' });
  treeBody.textContent = pickTreeText(result) || t('art.scanning');

  const copyBtn = h('button', {
    className: 'btn btn--ghost btn--sm',
    onClick: (e) => {
      e.stopPropagation();
      const text = treeBody.textContent || '';
      if (!text || text === t('art.scanning')) return;
      if (navigator?.clipboard?.writeText) {
        navigator.clipboard.writeText(text).then(() => {
          copyBtn.textContent = t('art.copied');
          setTimeout(() => { copyBtn.textContent = t('art.copyTree'); }, 1500);
        }).catch(() => { /* clipboard blocked */ });
      }
    },
  }, t('art.copyTree'));

  const card = h('div', { className: `chat-card chat-card--tree project-tree-card${status === 'running' ? ' chat-card--running' : ''}` }, [
    h('div', { className: 'chat-card__header' }, [
      h('span', { innerHTML: icon('git-branch'), className: 'chat-card__icon chat-card__icon--tree' }),
      h('span', { className: 'chat-card__label' }, subtitle ? t('art.projectTreeKinds', { kinds: subtitle }) : t('art.projectTree')),
      createStatusBadge(status),
    ]),
    h('div', { className: 'chat-card__body project-tree-card__body' }, [treeBody]),
    h('div', { className: 'chat-card__actions' }, [copyBtn]),
  ]);

  makeCollapsible(card);

  // Custom updater so we can swap the tree body in on completion instead of
  // bouncing through the generic status setter.
  const baseUpdater = bindStatusUpdater(card);
  return {
    element: card,
    updateStatus(newStatus, newResult) {
      const text = pickTreeText(newResult);
      if (text) treeBody.textContent = text;
      baseUpdater(newStatus);
    },
  };
}

// Tool results arrive as the standard envelope {success, data:{tree:"…"}};
// fall back to a bare {tree:"…"} for resilience against future shape changes.
function pickTreeText(result) {
  if (!result) return '';
  try {
    const parsed = typeof result === 'string' ? JSON.parse(result) : result;
    const tree = parsed?.data?.tree ?? parsed?.tree;
    if (typeof tree === 'string') return tree;
  } catch { /* not JSON */ }
  return '';
}
