import { h } from '../../../core/dom.js';
import { t } from '../../../core/i18n.js';
import { icon } from '../../icon.js';
import {
  switchPanel,
  getToolLabel,
  bindStatusUpdater,
  createStatusBadge,
  makeCollapsible,
} from './shared.js';

export function createTableCard(toolName, status, args, result) {
  const tableName = args?.table_name || args?.table || 'table';

  const card = h('div', { className: `chat-card chat-card--table${status === 'running' ? ' chat-card--running' : ''}` }, [
    h('div', { className: 'chat-card__header' }, [
      h('span', { innerHTML: icon('database'), className: 'chat-card__icon chat-card__icon--table' }),
      h('span', { className: 'chat-card__label' }, getToolLabel(toolName, args)),
      createStatusBadge(status),
    ]),
    h('div', { className: 'chat-card__body' }, [
      h('strong', {}, tableName),
    ]),
    h('div', { className: 'chat-card__actions' }, [
      h('button', {
        className: 'btn btn--ghost btn--sm',
        onClick: () => switchPanel('tables'),
      }, t('chat.viewTables')),
    ]),
  ]);

  makeCollapsible(card);
  return { element: card, updateStatus: bindStatusUpdater(card) };
}
