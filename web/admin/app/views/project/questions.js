/**
 * Project-scoped questions panel.
 * Groups pending questions into a single form with one submit button.
 */

import { h, clear } from '../../core/dom.js';
import { t, tn } from '../../core/i18n.js';
import { get, post } from '../../core/http.js';
import { createScope } from '../../core/lifecycle.js';
import { setPending } from '../../core/questions.js';
import * as state from '../../core/state.js';
import * as toast from '../../ui/toast.js';
import { emptyState, showLoadError } from '../../ui/helpers.js';
import { createGroupedForm, createApprovalCard, createAnsweredCard, createDeferredCard } from '../../ui/question-cards.js';
import { pageHeader } from '../../ui/card.js';
import { humanizeError } from '../../ui/errors.js';

export async function renderProjectQuestions(container, projectId) {
  clear(container);
  const scope = createScope();

  const header = pageHeader({
    title: t('nav.questions'),
  });

  const body = h('div', { className: 'context-panel__body' });
  container.appendChild(header);
  container.appendChild(body);

  // Set once the person has typed into the form, so a live refresh never
  // rebuilds it under their hands.
  let dirty = false;
  body.addEventListener('input', () => { dirty = true; });

  async function fetchQuestions() {
    return get(`/admin/api/projects/${projectId}/questions`);
  }

  // Re-read the list and the badge from the server's state.
  async function refresh() {
    const questions = await fetchQuestions();
    if (scope.disposed) return;
    setPending(projectId, questions.filter(q => q.status === 'pending').length);
    renderList(questions);
  }

  async function initialLoad() {
    try {
      await refresh();
    } catch (err) {
      if (scope.disposed) return;
      showLoadError(body, t('questions.loadFailed'), err, initialLoad);
    }
  }

  // A question can arrive (or be answered elsewhere, such as in the chat) while
  // this tab is open. Rebuilding the form would discard anything typed, so if
  // the person has started answering, offer the update instead of forcing it.
  function onRemoteChange(evt) {
    if (!evt || evt.project_id !== projectId) return;
    if (!dirty) {
      refresh().catch(() => { /* the next change or a reload will catch up */ });
      return;
    }
    if (body.querySelector('.questions-stale')) return;
    const bar = h('div', { className: 'form-hint form-hint--info questions-stale', role: 'status' }, [
      h('span', {}, t('pq.changed') + ' '),
      h('button', {
        className: 'btn btn--ghost btn--sm',
        type: 'button',
        onClick: () => refresh().catch((err) => toast.error(humanizeError(err.message))),
      }, t('pq.refresh')),
    ]);
    body.prepend(bar);
  }
  state.select('questionAsked', onRemoteChange, scope);
  state.select('questionAnswered', onRemoteChange, scope);

  function renderList(questions) {
    clear(body);
    dirty = false;

    if (questions.length === 0) {
      body.appendChild(emptyState({
        iconName: 'help-circle',
        title: t('questions.empty.title'),
        description: t('pq.empty.description'),
      }));
      return;
    }

    const pending = questions.filter(q => q.status === 'pending');
    const answered = questions.filter(q => q.status !== 'pending');
    const pendingRegular = pending.filter(q => q.type !== 'approval');
    const pendingApproval = pending.filter(q => q.type === 'approval');

    if (pendingRegular.length > 0) {
      body.appendChild(createGroupedForm(pendingRegular, {
        onSubmit: async (answers) => {
          let succeeded = 0;
          let failed = 0;
          for (const a of answers) {
            try {
              const payload = a.deferred ? { deferred: true } : { answer: a.answer };
              payload.project_id = projectId; // route to this project; ids collide across projects
              await post(`/admin/api/questions/${a.questionId}/answer`, payload);
              succeeded++;
            } catch (err) {
              failed++;
              // Counted in `failed` and reported below.
            }
          }
          if (succeeded > 0) toast.success(tn('questions.answersSubmitted', succeeded));
          if (failed > 0) toast.error(tn('questions.answersFailed', failed));
          // Refresh from the server's state. A failure here must not turn a
          // successful submit into an error message.
          try {
            await refresh();
          } catch (err) {
            toast.warning(t('pq.refreshFailed', { error: humanizeError(err.message) }));
          }
        },
      }));
    }

    for (const q of pendingApproval) {
      body.appendChild(createApprovalCard(q, { onRespond: submitSingle, onSkip: skipSingle }));
    }

    for (const q of answered) {
      body.appendChild(q.status === 'deferred'
        ? createDeferredCard(q, { onAnswer: submitSingle })
        : createAnsweredCard(q));
    }
  }

  async function submitSingle(questionId, answer) {
    try {
      await post(`/admin/api/questions/${questionId}/answer`, { answer, project_id: projectId });
      toast.success(t('questions.responseSubmitted'));
      await refresh();
    } catch (err) {
      toast.error(humanizeError(err.message));
    }
  }

  async function skipSingle(questionId) {
    try {
      await post(`/admin/api/questions/${questionId}/answer`, { deferred: true, project_id: projectId });
      toast.success(t('questions.deferred'));
      await refresh();
    } catch (err) {
      toast.error(humanizeError(err.message));
    }
  }

  await initialLoad();
  return () => scope.dispose();
}
