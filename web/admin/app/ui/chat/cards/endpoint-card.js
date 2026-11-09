import { h } from '../../../core/dom.js';
import { icon } from '../../icon.js';
import {
  getToolLabel,
  bindStatusUpdater,
  createStatusBadge,
  makeCollapsible,
} from './shared.js';

export function createEndpointCard(toolName, status, args, result) {
  const path = args?.endpoint_path || 'endpoint';
  const tableName = args?.table_name || '';
  const method = args?.method ? args.method.toUpperCase() : '';
  const card = h('div', { className: `chat-card chat-card--endpoint${status === 'running' ? ' chat-card--running' : ''}` }, [
    h('div', { className: 'chat-card__header' }, [
      h('span', { innerHTML: icon('zap'), className: 'chat-card__icon chat-card__icon--endpoint' }),
      h('span', { className: 'chat-card__label' }, getToolLabel(toolName, args)),
      createStatusBadge(status),
    ]),
    h('div', { className: 'chat-card__body' }, [
      method ? h('span', { className: 'badge badge--sm' }, method) : null,
      h('code', { style: { fontSize: '0.85rem' } }, `/api/${path}`),
      tableName ? h('span', { className: 'text-sm text-secondary ml-2' }, `-> ${tableName}`) : null,
    ].filter(Boolean)),
  ]);

  makeCollapsible(card);
  return { element: card, updateStatus: bindStatusUpdater(card) };
}
