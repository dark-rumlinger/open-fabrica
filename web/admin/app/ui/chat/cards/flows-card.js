import { h } from '../../../core/dom.js';
import { t, tn } from '../../../core/i18n.js';
import { icon } from '../../icon.js';
import { disclosure } from '../../a11y.js';
import { flowsView } from './flows-view-model.js';

const ROW_ICON = { passed: 'check', failed: 'x', blocked: 'clock', pending: 'clock' };

const BADGE_TONE = { success: 'success', danger: 'danger', warning: 'warning', info: 'info', neutral: 'secondary' };

function renderBar(rows) {
  if (!rows.length) return null;
  return h('div', { className: 'flows-bar', role: 'img', 'aria-label': tn('chat.flowsCount', rows.length) },
    rows.map((r) => h('span', { className: `flows-bar__seg flows-bar__seg--${r.status}`, title: r.name })));
}

function renderRow(r) {
  const main = h('div', { className: 'flows-row__main' }, [
    h('span', { className: `flows-row__icon flows-row__icon--${r.status}`, innerHTML: icon(ROW_ICON[r.status] || 'clock') }),
    h('span', { className: 'flows-row__name' }, r.name),
    h('span', { className: 'flows-row__detail' }, r.detail),
  ]);
  const children = [main];
  if (r.reason) children.push(h('div', { className: 'flows-row__reason' }, r.reason));
  if (r.full) {
    children.push(h('details', { className: 'flows-row__more' }, [
      h('summary', {}, t('chat.fullReport')),
      h('pre', {}, r.full),
    ]));
  }
  return h('div', { className: `flows-row flows-row--${r.status}` }, children);
}

/**
 * Result card for the user flow actions of manage_testing (write_flows and
 * test_flows). The header matches the other tool cards: expand arrow, small icon,
 * label and a status tag at the end. Below it: a summary line, a bar with one
 * segment per flow and one row per flow. The outcome is read from the result, not
 * the tool-call status, because history reloads every call as a success.
 */
export function createFlowsCard(toolName, status, args, result) {
  const action = args?.action;
  const label = h('span', { className: 'chat-card__label' });
  const badgeSlot = h('span', { className: 'flows-card__badge' });
  const header = h('div', { className: 'chat-card__header' }, [
    h('span', { className: 'chat-card__chevron', innerHTML: icon('chevron-right') }),
    h('span', { className: 'chat-card__icon chat-card__icon--flows', innerHTML: icon('activity') }),
    label,
    badgeSlot,
  ]);
  const detail = h('div', { className: 'chat-card__detail flows-card__detail' });
  const card = h('div', { className: 'chat-card chat-card--flows' }, [header, detail]);

  disclosure(header, () => {
    header.classList.toggle('expanded');
    return detail.classList.toggle('visible');
  });

  function apply(view) {
    const running = view.state === 'running';
    card.className = `chat-card chat-card--flows chat-card--flows-${view.tone}${running ? ' chat-card--running' : ''}`;
    label.textContent = view.title;
    badgeSlot.replaceChildren();
    if (running) {
      badgeSlot.appendChild(h('span', { className: 'badge badge--info' }, [h('span', { className: 'spinner spinner--sm' }), ' ' + t('tasks.status.running')]));
    } else if (view.badge) {
      badgeSlot.appendChild(h('span', { className: `badge badge--${BADGE_TONE[view.tone] || 'secondary'}` }, view.badge));
    }

    const body = [];
    if (view.sub) body.push(h('div', { className: 'flows-card__sub' }, view.sub));
    if (view.saved && view.saved.flows) {
      body.push(h('div', { className: 'flows-card__saved' },
        view.saved.steps
          ? t('chat.savedWithSteps', { flows: tn('chat.flowsCount', view.saved.flows), steps: tn('chat.stepsCount', view.saved.steps) })
          : t('chat.saved', { flows: tn('chat.flowsCount', view.saved.flows) })));
    }
    body.push(renderBar(view.rows));
    if (view.rows.length) body.push(h('div', { className: 'flows-card__rows' }, view.rows.map(renderRow)));
    if (view.notes.length) {
      body.push(h('ul', { className: 'flows-card__notes' }, view.notes.map((n) => h('li', {}, n))));
    }
    detail.replaceChildren(...body.filter(Boolean));
    header.classList.toggle('expanded', view.expanded);
    detail.classList.toggle('visible', view.expanded);
  }

  apply(flowsView(action, args, result, status));

  return {
    element: card,
    updateStatus(newStatus, newResult) {
      apply(flowsView(action, args, newResult, newStatus));
      const flash = card.classList.contains('chat-card--flows-success') ? 'chat-card--success-flash' : 'chat-card--error-flash';
      card.classList.add(flash);
      setTimeout(() => card.classList.remove(flash), 700);
    },
  };
}
