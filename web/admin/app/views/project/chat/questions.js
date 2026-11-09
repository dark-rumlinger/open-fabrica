import { h } from '../../../core/dom.js';
import { t, tn } from '../../../core/i18n.js';
import { icon } from '../../../ui/icon.js';
import { createQuestionCard, createQuestionGroup } from '../../../ui/question-cards.js';

/**
 * Single owner of pending-question card state for the chat view. Renders the
 * pinned banner and reconciles brain.questionAsked / questionAnswered events
 * with the existing card map. Single cleanup path, no double handlers.
 */
export function createQuestionManager(feed, scope, projectId) {
  const cards = {};        // questionId -> card-or-group controller
  const data = {};         // questionId -> question payload (for banner copy)
  let activeGroup = null;  // active multi-question group

  // Stamp the chat's project id onto every question payload. Answer POSTs need
  // it so the backend routes to THIS project's question, not a colliding id in
  // another project (question ids are per-project). Event/list payloads don't
  // always carry it, so set it authoritatively from the chat we're rendering.
  const stamp = (q) => (q && q.project_id == null && projectId != null ? { ...q, project_id: projectId } : q);

  const banner = h('div', { className: 'chat-question-banner' });
  banner.style.display = 'none';

  function update() {
    const ids = Object.keys(cards);
    if (ids.length === 0) {
      banner.style.display = 'none';
      return;
    }
    banner.style.display = '';
    banner.innerHTML = '';

    const count = ids.length;
    const badgeEl = h('span', { className: 'badge badge--warning' },
      count > 1 ? tn('qc.count', count) : t('qc.question'));

    const textEl = h('div', { className: 'chat-question-banner__text' },
      count > 1 ? tn('chat.quickQuestions', count) : (data[ids[0]]?.question || t('chat.quickQuestion')));

    const scrollBtn = h('button', {
      className: 'btn btn--sm btn--primary',
      onClick: () => {
        const target = activeGroup?.element || cards[ids[ids.length - 1]]?.element;
        if (target) target.scrollIntoView({ behavior: 'smooth', block: 'center' });
      },
    }, t('chat.answer'));

    banner.appendChild(h('span', { innerHTML: icon('help-circle'), className: 'chat-question-banner__icon' }));
    banner.appendChild(badgeEl);
    banner.appendChild(textEl);
    banner.appendChild(scrollBtn);
  }

  function add(q) {
    q = stamp(q);
    if (cards[q.id]) return;
    if (q.type === 'approval') {
      const card = createQuestionCard(q);
      cards[q.id] = card;
      data[q.id] = q;
      feed.append(card.element);
    } else if (activeGroup) {
      activeGroup.addQuestion(q);
      cards[q.id] = activeGroup;
      data[q.id] = q;
    } else {
      activeGroup = createQuestionGroup([q]);
      cards[q.id] = activeGroup;
      data[q.id] = q;
      feed.append(activeGroup.element);
    }
    update();
  }

  function remove(qId) {
    if (!cards[qId]) return;
    delete cards[qId];
    delete data[qId];
    if (activeGroup?.questionIds) {
      const stillPending = [...activeGroup.questionIds].some(id => cards[id]);
      if (!stillPending) activeGroup = null;
    }
    update();
  }

  function loadInitial(questions) {
    if (!questions || questions.length === 0) return false;
    questions = questions.map(stamp);
    const approval = questions.filter(q => q.type === 'approval');
    const regular = questions.filter(q => q.type !== 'approval');

    if (regular.length > 0) {
      activeGroup = createQuestionGroup(regular);
      for (const q of regular) {
        cards[q.id] = activeGroup;
        data[q.id] = q;
      }
      feed.append(activeGroup.element);
    }
    for (const q of approval) {
      if (!cards[q.id]) {
        const card = createQuestionCard(q);
        cards[q.id] = card;
        data[q.id] = q;
        feed.append(card.element);
      }
    }
    feed.scrollToBottom();
    update();
    return true;
  }

  function removeFromCard(qId) {
    const c = cards[qId];
    if (c?.element && c !== activeGroup) c.element.remove();
    remove(qId);
  }

  // External events: cards.js dispatches `of:questionAnswered` after a user
  // answers a card; questionAnswered state events fire when answered elsewhere.
  function onAnswered(e) { remove(e.detail.questionId); }
  document.addEventListener('of:questionAnswered', onAnswered);
  if (scope) scope.add(() => document.removeEventListener('of:questionAnswered', onAnswered));

  return {
    banner,
    add,
    remove: removeFromCard,
    loadInitial,
    get hasPending() { return Object.keys(cards).length > 0; },
  };
}
